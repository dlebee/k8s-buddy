package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/metadata"
)

// ObjectDetail is a resource as JSON plus the related objects the drawer summaries need
// (pods a workload or service selects, a release's revision history, ...).
type ObjectDetail struct {
	Cluster   string         `json:"cluster"`
	Kind      string         `json:"kind"`
	Namespace string         `json:"namespace,omitempty"`
	Name      string         `json:"name"`
	Object    map[string]any `json:"object,omitempty"`
	Pods      []PodBrief     `json:"pods,omitempty"`
	// Services referenced by an ingress: name → ports (nil when the service doesn't exist).
	Services map[string][]int32 `json:"services,omitempty"`
	// BackendPods are the pods each of an ingress's services selects (absent: no selector).
	BackendPods map[string][]PodBrief `json:"backendPods,omitempty"`
	// SecretSizes is the decoded size in bytes of each key of a Secret (values themselves are removed).
	SecretSizes map[string]int `json:"secretSizes,omitempty"`
	History     []HelmRevision `json:"history,omitempty"`
	// PodCount is the number of pods scheduled on a node.
	PodCount *int `json:"podCount,omitempty"`
	// Disks are the volumes attached to a node, against the attach limits its drivers advertise.
	Disks *NodeDisks `json:"disks,omitempty"`
}

// PodBrief is just enough of a pod to draw a health dot and offer shell/port-forward.
type PodBrief struct {
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	Health     Health   `json:"health"`
	Ready      string   `json:"ready"`
	Restarts   int32    `json:"restarts"`
	Node       string   `json:"node,omitempty"`
	Containers []string `json:"containers"`
	Ports      []int32  `json:"ports,omitempty"`
}

type HelmRevision struct {
	Revision int       `json:"revision"`
	Status   string    `json:"status"`
	Updated  time.Time `json:"updated"`
}

func podBrief(p *corev1.Pod) PodBrief {
	status, h := podStatus(p)
	ready, restarts := 0, int32(0)
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
		restarts += cs.RestartCount
	}
	b := PodBrief{Name: p.Name, Status: status, Health: h, Ready: fmt.Sprintf("%d/%d", ready, len(p.Spec.Containers)),
		Restarts: restarts, Node: p.Spec.NodeName, Containers: []string{}}
	for _, c := range p.Spec.Containers {
		b.Containers = append(b.Containers, c.Name)
		for _, port := range c.Ports {
			if port.Protocol == "" || port.Protocol == corev1.ProtocolTCP {
				b.Ports = append(b.Ports, port.ContainerPort)
			}
		}
	}
	return b
}

func (c *cluster) podsMatching(ns string, sel labels.Selector) []PodBrief {
	out := []PodBrief{}
	if !c.synced("pods") || sel.Empty() {
		return out
	}
	pods, _ := c.pods.Pods(ns).List(sel)
	for _, p := range pods {
		out = append(out, podBrief(p))
	}
	slices.SortFunc(out, func(a, b PodBrief) int {
		if r := healthRank(a.Health) - healthRank(b.Health); r != 0 {
			return r
		}
		if a.Name < b.Name {
			return -1
		}
		return 1
	})
	return out
}

func healthRank(h Health) int {
	return map[Health]int{HealthBad: 0, HealthWarn: 1, HealthNeutral: 2, HealthOK: 3}[h]
}

func toMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	err = json.Unmarshal(b, &m)
	return m, err
}

// Object returns a resource as JSON with secret values removed, plus related objects.
func (m *Manager) Object(clusterName, kind, ns, name string) (ObjectDetail, error) {
	d := ObjectDetail{Cluster: clusterName, Kind: kind, Namespace: ns, Name: name}
	c, err := m.cluster(clusterName)
	if err != nil {
		return d, err
	}
	if kind == "helm" {
		return c.helmHistory(d)
	}
	if spec, ok := kinds[kind]; ok && spec.get != nil {
		if lazyKinds[kind] {
			c.ensureLazy(spec.resource)
		}
		if !c.synced(spec.resource) {
			if msg := c.resourceError(spec.resource); msg != "" {
				return d, fmt.Errorf("%s", msg)
			}
			return d, fmt.Errorf("%s are still loading on %s", spec.label, clusterName)
		}
		obj, _, err := spec.get(c, ns, name)
		if err != nil {
			return d, err
		}
		obj = obj.DeepCopyObject()
		if sec, ok := obj.(*corev1.Secret); ok {
			hidden := map[string]string{}
			for _, k := range slices.Sorted(maps.Keys(sec.Data)) {
				hidden[k] = "<hidden by k8s-buddy>"
			}
			sec.Data, sec.StringData = nil, hidden
		}
		if d.Object, err = toMap(obj); err != nil {
			return d, err
		}
		c.related(&d, obj)
	} else if g, ok := genericFor(kind); ok {
		res, ok := c.findResource(g.res.Group, g.res.Resource)
		if !ok {
			return d, fmt.Errorf("%s not served by %s", g.res.Resource, clusterName)
		}
		p, err := res.objectPath(ns, name)
		if err != nil {
			return d, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		raw, err := c.cs.CoreV1().RESTClient().Get().AbsPath(p).DoRaw(ctx)
		if err != nil {
			return d, err
		}
		if err := json.Unmarshal(raw, &d.Object); err != nil {
			return d, err
		}
		if err := singleObject(d.Object); err != nil {
			d.Object = nil
			return d, err
		}
		if res.Group == "" && res.Resource == "secrets" {
			d.SecretSizes = secretSizes(d.Object)
		}
		redactObject(d.Object)
		if res.Group == "networking.k8s.io" && res.Resource == "ingresses" {
			c.ingressBackends(&d)
		}
		if res.Group == gatewayGroup && strings.HasSuffix(res.Resource, "routes") {
			c.routeBackends(&d)
		}
	} else {
		return d, fmt.Errorf("unknown kind %q", kind)
	}
	if md, ok := d.Object["metadata"].(map[string]any); ok {
		delete(md, "managedFields")
		if ann, ok := md["annotations"].(map[string]any); ok {
			delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
		}
	}
	return d, nil
}

func (c *cluster) related(d *ObjectDetail, obj any) {
	selector := func(ls *metav1.LabelSelector) labels.Selector {
		s, err := metav1.LabelSelectorAsSelector(ls)
		if err != nil {
			return labels.Nothing()
		}
		return s
	}
	switch o := obj.(type) {
	case *appsv1.Deployment:
		d.Pods = c.podsMatching(o.Namespace, selector(o.Spec.Selector))
	case *appsv1.StatefulSet:
		d.Pods = c.podsMatching(o.Namespace, selector(o.Spec.Selector))
	case *appsv1.DaemonSet:
		d.Pods = c.podsMatching(o.Namespace, selector(o.Spec.Selector))
	case *corev1.Service:
		if len(o.Spec.Selector) > 0 {
			d.Pods = c.podsMatching(o.Namespace, labels.SelectorFromSet(o.Spec.Selector))
		} else {
			d.Pods = []PodBrief{}
		}
	case *corev1.Node:
		n := 0
		if c.synced("pods") {
			pods, _ := c.pods.List(labels.Everything())
			for _, p := range pods {
				if p.Spec.NodeName == o.Name && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
					n++
				}
			}
		}
		d.PodCount = &n
		disks := nodeDisks(o, c.csiNode(o.Name))
		d.Disks = &disks
	case *corev1.PersistentVolumeClaim:
		d.Pods = []PodBrief{}
		if c.synced("pods") {
			pods, _ := c.pods.Pods(o.Namespace).List(labels.Everything())
			for _, p := range pods {
				for _, v := range p.Spec.Volumes {
					if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == o.Name {
						d.Pods = append(d.Pods, podBrief(p))
						break
					}
				}
			}
		}
	}
}

func (c *cluster) ingressBackends(d *ObjectDetail) {
	names := map[string]bool{}
	spec, _ := d.Object["spec"].(map[string]any)
	addBackend := func(b any) {
		if s := str(dig(asMap(b), "service", "name")); s != "" {
			names[s] = true
		}
	}
	addBackend(spec["defaultBackend"])
	rules, _ := spec["rules"].([]any)
	for _, r := range rules {
		paths, _ := dig(asMap(r), "http", "paths").([]any)
		for _, p := range paths {
			addBackend(asMap(p)["backend"])
		}
	}
	d.Services = map[string][]int32{}
	d.BackendPods = map[string][]PodBrief{}
	if !c.synced("services") {
		return
	}
	for n := range names {
		svc, err := c.services.Services(d.Namespace).Get(n)
		if err != nil {
			d.Services[n] = nil
			continue
		}
		ports := []int32{}
		for _, p := range svc.Spec.Ports {
			ports = append(ports, p.Port)
		}
		d.Services[n] = ports
		if len(svc.Spec.Selector) > 0 {
			d.BackendPods[n] = c.podsMatching(d.Namespace, labels.SelectorFromSet(svc.Spec.Selector))
		}
	}
}

// routeBackends fills Services/BackendPods for a Gateway API route. Keys are the service name, or
// "namespace/name" for a backend in another namespace.
func (c *cluster) routeBackends(d *ObjectDetail) {
	r := parseRoute("", d.Object)
	d.Services = map[string][]int32{}
	d.BackendPods = map[string][]PodBrief{}
	if !c.synced("services") {
		return
	}
	for _, rule := range r.Rules {
		for _, b := range rule.Backends {
			if b.Kind != "Service" || b.Name == "" {
				continue
			}
			key := b.Name
			if b.Namespace != d.Namespace {
				key = b.Namespace + "/" + b.Name
			}
			svc, err := c.services.Services(b.Namespace).Get(b.Name)
			if err != nil {
				d.Services[key] = nil
				continue
			}
			ports := []int32{}
			for _, p := range svc.Spec.Ports {
				ports = append(ports, p.Port)
			}
			d.Services[key] = ports
			if len(svc.Spec.Selector) > 0 {
				d.BackendPods[key] = c.podsMatching(b.Namespace, labels.SelectorFromSet(svc.Spec.Selector))
			}
		}
	}
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// helmHistory asks the API server for this release's storage secrets when the release is opened —
// metadata only (labels carry revision, status and time), never the release payload itself.
func (c *cluster) helmHistory(d ObjectDetail) (ObjectDetail, error) {
	if !validHelmName(d.Name) || !validObjectName(d.Namespace) {
		return d, fmt.Errorf("invalid release name")
	}
	mc, err := metadata.NewForConfig(c.cfg)
	if err != nil {
		return d, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	list, err := mc.Resource(corev1.SchemeGroupVersion.WithResource("secrets")).Namespace(d.Namespace).
		List(ctx, metav1.ListOptions{LabelSelector: "owner=helm,name=" + d.Name})
	if apierrors.IsForbidden(err) {
		return d, fmt.Errorf("no access to Helm's release records (listing secrets in %s is not allowed), so revision history is unavailable", d.Namespace)
	}
	if err != nil {
		return d, err
	}
	for i := range list.Items {
		m := &list.Items[i]
		rev, _ := strconv.Atoi(m.Labels["version"])
		r := HelmRevision{Revision: rev, Status: m.Labels["status"], Updated: m.CreationTimestamp.Time}
		if ts, err := strconv.ParseInt(m.Labels["modifiedAt"], 10, 64); err == nil {
			r.Updated = time.Unix(ts, 0)
		}
		d.History = append(d.History, r)
	}
	if len(d.History) == 0 {
		return d, fmt.Errorf("release %s/%s not found", d.Namespace, d.Name)
	}
	slices.SortFunc(d.History, func(a, b HelmRevision) int { return b.Revision - a.Revision })
	return d, nil
}

// PodContainers lists a pod's (non-init) containers, used to validate shell requests.
func (m *Manager) PodContainers(clusterName, ns, name string) ([]string, error) {
	c, err := m.cluster(clusterName)
	if err != nil {
		return nil, err
	}
	p, err := c.pods.Pods(ns).Get(name)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(p.Spec.Containers))
	for _, ct := range p.Spec.Containers {
		out = append(out, ct.Name)
	}
	return out, nil
}

// Connected reports whether a cluster with this context name is being monitored.
func (m *Manager) Connected(clusterName string) bool {
	_, err := m.cluster(clusterName)
	return err == nil
}
