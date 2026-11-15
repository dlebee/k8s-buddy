package monitor

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

type Health string

const (
	HealthOK      Health = "ok"
	HealthWarn    Health = "warn"
	HealthBad     Health = "bad"
	HealthNeutral Health = "neutral"
)

type Resource struct {
	Cluster   string    `json:"cluster"`
	Namespace string    `json:"namespace,omitempty"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	Health    Health    `json:"health"`
	Created   time.Time `json:"created"`
	Cols      []string  `json:"cols"`
}

type kindSpec struct {
	resource   string // informer name
	label      string
	namespaced bool
	columns    []string
	list       func(c *cluster) []Resource
	get        func(c *cluster, ns, name string) (runtime.Object, string, error)
}

// Columns roughly follow `kubectl get` / k9s for each kind.
var lazyKinds = map[string]bool{"configmaps": true, "secrets": true}

var kinds = map[string]kindSpec{
	"pods": {"pods", "Pods", true, []string{"Ready", "Restarts", "Node", "IP"}, listPods,
		func(c *cluster, ns, n string) (runtime.Object, string, error) {
			o, err := c.pods.Pods(ns).Get(n)
			return o, "Pod", err
		}},
	"deployments": {"deployments", "Deployments", true, []string{"Ready", "Up-to-date", "Available", "Images"}, listDeployments,
		func(c *cluster, ns, n string) (runtime.Object, string, error) {
			o, err := c.deploys.Deployments(ns).Get(n)
			return o, "Deployment", err
		}},
	"statefulsets": {"statefulsets", "StatefulSets", true, []string{"Ready", "Images"}, listStatefulSets,
		func(c *cluster, ns, n string) (runtime.Object, string, error) {
			o, err := c.sts.StatefulSets(ns).Get(n)
			return o, "StatefulSet", err
		}},
	"daemonsets": {"daemonsets", "DaemonSets", true, []string{"Desired", "Ready", "Up-to-date", "Available"}, listDaemonSets,
		func(c *cluster, ns, n string) (runtime.Object, string, error) {
			o, err := c.ds.DaemonSets(ns).Get(n)
			return o, "DaemonSet", err
		}},
	"services": {"services", "Services", true, []string{"Type", "Cluster IP", "External IP", "Ports"}, listServices,
		func(c *cluster, ns, n string) (runtime.Object, string, error) {
			o, err := c.services.Services(ns).Get(n)
			return o, "Service", err
		}},
	"pvcs": {"pvcs", "Volume claims", true, []string{"Volume", "Capacity", "Storage class"}, listPVCs,
		func(c *cluster, ns, n string) (runtime.Object, string, error) {
			o, err := c.pvcs.PersistentVolumeClaims(ns).Get(n)
			return o, "PersistentVolumeClaim", err
		}},
	"nodes": {"nodes", "Nodes", false, []string{"Roles", "Pool", "Version", "OS image"}, listNodes,
		func(c *cluster, _, n string) (runtime.Object, string, error) {
			o, err := c.nodes.Get(n)
			return o, "Node", err
		}},
	"configmaps": {"configmaps", "ConfigMaps", true, []string{"Keys"}, listConfigMaps,
		func(c *cluster, ns, n string) (runtime.Object, string, error) {
			o, err := c.configmaps.ConfigMaps(ns).Get(n)
			return o, "ConfigMap", err
		}},
	"secrets": {"secrets", "Secrets", true, []string{"Type", "Keys"}, listSecrets,
		func(c *cluster, ns, n string) (runtime.Object, string, error) {
			o, err := c.secrets.Secrets(ns).Get(n)
			return o, "Secret", err
		}},
	"helm":   {"helm", "Helm releases", true, []string{"Revision", "Updated"}, listHelm, nil},
	"events": {"events", "Warning events", true, []string{"Object", "Message", "Count", "Last seen"}, listEvents, nil},
}

type KindInfo struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Section string   `json:"section"`
	Columns []string `json:"columns,omitempty"`
}

var kindSections = []struct {
	name string
	ids  []string
}{
	{"Workloads", []string{"pods", "deployments", "statefulsets", "daemonsets", "jobs", "cronjobs", "hpas"}},
	{"Network", []string{"services", "ingresses", "networkpolicies"}},
	{"Storage", []string{"pvcs", "pvs", "storageclasses"}},
	{"Config", []string{"configmaps", "secrets"}},
	{"Cluster", []string{"nodes", "namespaces", "pdbs", "events"}},
	{"Helm", []string{"helm"}},
}

func Kinds() []KindInfo {
	var out []KindInfo
	for _, sec := range kindSections {
		for _, id := range sec.ids {
			if k, ok := kinds[id]; ok {
				out = append(out, KindInfo{ID: id, Label: k.label, Section: sec.name, Columns: k.columns})
			} else {
				out = append(out, KindInfo{ID: id, Label: genericBuiltins[id].label, Section: sec.name})
			}
		}
	}
	return out
}

type ResourceQuery struct {
	Kind       string
	Clusters   []string // empty means all
	Namespaces []string // empty means all; cluster-scoped kinds ignore it
	Search     string
	Problems   bool
	Limit      int
}

type ResourceList struct {
	Kind        string        `json:"kind"`
	Columns     []string      `json:"columns"`
	Namespaced  bool          `json:"namespaced"`
	Items       []Resource    `json:"items"`
	Total       int           `json:"total"`
	Problems    int           `json:"problems"`
	Namespaces  []string      `json:"namespaces"`
	Unavailable []Unavailable `json:"unavailable"`
}

type Unavailable struct {
	Cluster string `json:"cluster"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail,omitempty"`
}

func unavailable(c *cluster, resource string) Unavailable {
	msg := c.resourceError(resource)
	switch {
	case msg == "":
		return Unavailable{Cluster: c.name, Reason: "loading"}
	case strings.Contains(msg, "forbidden"):
		return Unavailable{Cluster: c.name, Reason: "no access", Detail: msg}
	default:
		return Unavailable{Cluster: c.name, Reason: "error", Detail: msg}
	}
}

func (m *Manager) connected() []*cluster {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*cluster
	for _, r := range m.running {
		out = append(out, r.c)
	}
	slices.SortFunc(out, func(a, b *cluster) int { return strings.Compare(a.name, b.name) })
	return out
}

func (m *Manager) Resources(q ResourceQuery) (ResourceList, error) {
	if g, ok := genericFor(q.Kind); ok {
		return m.genericResources(q, g), nil
	}
	spec, ok := kinds[q.Kind]
	if !ok {
		return ResourceList{}, fmt.Errorf("unknown kind %q", q.Kind)
	}
	out := newResourceList(q.Kind, spec.columns, spec.namespaced)
	var rows []Resource
	for _, c := range m.connected() {
		if !inScope(q.Clusters, c.name) {
			continue
		}
		if lazyKinds[q.Kind] {
			c.ensureLazy(spec.resource)
		}
		if !c.synced(spec.resource) {
			// Helm falls back to label-based discovery on workloads when the
			// storage secret watch is denied — if those are synced, serve
			// what we can derive without the secret.
			if q.Kind == "helm" && c.helmWorkloadFallbackReady() {
				for _, r := range listHelm(c) {
					r.Cluster = c.name
					rows = append(rows, r)
				}
				continue
			}
			out.Unavailable = append(out.Unavailable, unavailable(c, spec.resource))
			continue
		}
		for _, r := range spec.list(c) {
			r.Cluster = c.name
			rows = append(rows, r)
		}
	}
	out.apply(rows, q)
	return out, nil
}

func inScope(set []string, v string) bool {
	return len(set) == 0 || slices.Contains(set, v)
}

func newResourceList(kind string, columns []string, namespaced bool) ResourceList {
	return ResourceList{Kind: kind, Columns: columns, Namespaced: namespaced, Items: []Resource{}, Namespaces: []string{}, Unavailable: []Unavailable{}}
}

// apply filters, counts, sorts and truncates rows according to q.
func (out *ResourceList) apply(rows []Resource, q ResourceQuery) {
	nsSet := map[string]bool{}
	needle := strings.ToLower(q.Search)
	for _, r := range rows {
		if r.Namespace != "" {
			nsSet[r.Namespace] = true
		}
		if r.Namespace != "" && !inScope(q.Namespaces, r.Namespace) {
			continue
		}
		bad := r.Health == HealthBad || r.Health == HealthWarn
		if q.Problems && !bad {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(strings.Join(append([]string{r.Cluster, r.Namespace, r.Name, r.Status}, r.Cols...), " ")), needle) {
			continue
		}
		out.Total++
		if bad {
			out.Problems++
		}
		out.Items = append(out.Items, r)
	}
	slices.SortStableFunc(out.Items, func(a, b Resource) int {
		if q.Kind == "events" {
			return b.Created.Compare(a.Created)
		}
		return strings.Compare(a.Cluster+"/"+a.Namespace+"/"+a.Name, b.Cluster+"/"+b.Namespace+"/"+b.Name)
	})
	if q.Limit > 0 && len(out.Items) > q.Limit {
		out.Items = out.Items[:q.Limit]
	}
	for ns := range nsSet {
		out.Namespaces = append(out.Namespaces, ns)
	}
	slices.Sort(out.Namespaces)
}

type ResourceDetail struct {
	Cluster   string     `json:"cluster"`
	Kind      string     `json:"kind"`
	Namespace string     `json:"namespace,omitempty"`
	Name      string     `json:"name"`
	YAML      string     `json:"yaml"`
	Events    []Resource `json:"events"`
	Commands  []string   `json:"commands"`
}

func (m *Manager) Resource(clusterName, kind, ns, name string) (ResourceDetail, error) {
	if g, ok := genericFor(kind); ok {
		return m.genericDetail(clusterName, kind, g, ns, name)
	}
	if kind == "helm" {
		return m.helmDetail(clusterName, ns, name)
	}
	spec, ok := kinds[kind]
	if !ok || spec.get == nil {
		return ResourceDetail{}, fmt.Errorf("no detail view for %q", kind)
	}
	var c *cluster
	for _, x := range m.connected() {
		if x.name == clusterName {
			c = x
		}
	}
	if c == nil || !c.synced(spec.resource) {
		return ResourceDetail{}, fmt.Errorf("cluster %q is not connected", clusterName)
	}
	obj, k8sKind, err := spec.get(c, ns, name)
	if err != nil {
		return ResourceDetail{}, err
	}
	obj = obj.DeepCopyObject()
	apiVersion := "v1"
	if strings.HasSuffix(k8sKind, "Set") || k8sKind == "Deployment" {
		apiVersion = "apps/v1"
	}
	obj.GetObjectKind().SetGroupVersionKind(schema.FromAPIVersionAndKind(apiVersion, k8sKind))
	var secretKeys []string
	if sec, ok := obj.(*corev1.Secret); ok {
		secretKeys = slices.Sorted(maps.Keys(sec.Data))
		hidden := make(map[string]string, len(sec.Data))
		for _, k := range secretKeys {
			hidden[k] = "<hidden by k8s-buddy>"
		}
		sec.Data, sec.StringData = nil, hidden
	}
	b, err := yaml.Marshal(obj)
	if err != nil {
		return ResourceDetail{}, err
	}
	d := ResourceDetail{Cluster: clusterName, Kind: kind, Namespace: ns, Name: name, YAML: string(b), Events: []Resource{}}
	if c.synced("events") {
		evs, _ := c.events.List(labels.Everything())
		for _, e := range evs {
			if e.InvolvedObject.Kind == k8sKind && e.InvolvedObject.Name == name && e.InvolvedObject.Namespace == ns {
				d.Events = append(d.Events, eventRow(e))
			}
		}
		slices.SortFunc(d.Events, func(a, b Resource) int { return b.Created.Compare(a.Created) })
	}
	ref := strings.ToLower(k8sKind) + "/" + name
	d.Commands = []string{kubectl(clusterName, ns, "describe "+ref), kubectl(clusterName, ns, "get "+ref+" -o yaml")}
	if kind == "pods" {
		d.Commands = append(d.Commands, kubectl(clusterName, ns, "logs "+name+" --all-containers --tail=200"))
	} else if kind == "secrets" {
		for _, k := range secretKeys[:min(len(secretKeys), 5)] {
			d.Commands = append(d.Commands, kubectl(clusterName, ns, fmt.Sprintf("get secret %s -o go-template='{{index .data %q | base64decode}}'", name, k)))
		}
	} else if kind == "deployments" || kind == "statefulsets" || kind == "daemonsets" {
		d.Commands = append(d.Commands, kubectl(clusterName, ns, "rollout status "+ref), kubectl(clusterName, ns, "rollout restart "+ref))
	}
	return d, nil
}

func keyList(keys []string) string {
	slices.Sort(keys)
	return truncate(fmt.Sprintf("%d · %s", len(keys), strings.Join(keys, ", ")), 90)
}

func listConfigMaps(c *cluster) []Resource {
	cms, _ := c.configmaps.List(labels.Everything())
	out := make([]Resource, 0, len(cms))
	for _, cm := range cms {
		keys := slices.Collect(maps.Keys(cm.Data))
		keys = append(keys, slices.Collect(maps.Keys(cm.BinaryData))...)
		out = append(out, Resource{Namespace: cm.Namespace, Name: cm.Name, Status: "Active", Health: HealthNeutral,
			Created: cm.CreationTimestamp.Time, Cols: []string{keyList(keys)}})
	}
	return out
}

func listSecrets(c *cluster) []Resource {
	secs, _ := c.secrets.List(labels.Everything())
	out := make([]Resource, 0, len(secs))
	for _, s := range secs {
		out = append(out, Resource{Namespace: s.Namespace, Name: s.Name, Status: "Active", Health: HealthNeutral,
			Created: s.CreationTimestamp.Time, Cols: []string{string(s.Type), keyList(slices.Collect(maps.Keys(s.Data)))}})
	}
	return out
}

func listPods(c *cluster) []Resource {
	pods, _ := c.pods.List(labels.Everything())
	out := make([]Resource, 0, len(pods))
	for _, p := range pods {
		status, h := podStatus(p)
		ready, restarts := 0, int32(0)
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Ready {
				ready++
			}
			restarts += cs.RestartCount
		}
		out = append(out, Resource{
			Namespace: p.Namespace, Name: p.Name, Status: status, Health: h, Created: p.CreationTimestamp.Time,
			Cols: []string{fmt.Sprintf("%d/%d", ready, len(p.Spec.Containers)), fmt.Sprint(restarts), p.Spec.NodeName, p.Status.PodIP},
		})
	}
	return out
}

// podStatus mirrors the STATUS column logic of `kubectl get pods`.
func podStatus(p *corev1.Pod) (string, Health) {
	reason := string(p.Status.Phase)
	if p.Status.Reason != "" {
		reason = p.Status.Reason
	}
	initializing := false
	for i, cs := range p.Status.InitContainerStatuses {
		switch {
		case cs.State.Terminated != nil && cs.State.Terminated.ExitCode == 0:
			continue
		case cs.State.Terminated != nil:
			r := cs.State.Terminated.Reason
			if r == "" {
				r = fmt.Sprintf("ExitCode:%d", cs.State.Terminated.ExitCode)
			}
			reason = "Init:" + r
		case cs.State.Waiting != nil && cs.State.Waiting.Reason != "" && cs.State.Waiting.Reason != "PodInitializing":
			reason = "Init:" + cs.State.Waiting.Reason
		default:
			reason = fmt.Sprintf("Init:%d/%d", i, len(p.Spec.InitContainers))
		}
		initializing = true
		break
	}
	allReady := len(p.Status.ContainerStatuses) > 0
	if !initializing {
		hasRunning := false
		for i := len(p.Status.ContainerStatuses) - 1; i >= 0; i-- {
			cs := p.Status.ContainerStatuses[i]
			if !cs.Ready {
				allReady = false
			}
			switch {
			case cs.State.Waiting != nil && cs.State.Waiting.Reason != "":
				reason = cs.State.Waiting.Reason
			case cs.State.Terminated != nil && cs.State.Terminated.Reason != "":
				reason = cs.State.Terminated.Reason
			case cs.State.Terminated != nil:
				reason = fmt.Sprintf("ExitCode:%d", cs.State.Terminated.ExitCode)
			case cs.State.Running != nil && cs.Ready:
				hasRunning = true
			}
		}
		if reason == "Completed" && hasRunning {
			reason = "Running"
		}
	}
	if p.DeletionTimestamp != nil {
		return "Terminating", HealthWarn
	}
	switch {
	case reason == "Completed" || p.Status.Phase == corev1.PodSucceeded:
		return reason, HealthNeutral
	case badWaiting[reason] || strings.HasPrefix(reason, "Init:") && badWaiting[strings.TrimPrefix(reason, "Init:")],
		reason == "Error", reason == "OOMKilled", reason == "Evicted", p.Status.Phase == corev1.PodFailed,
		strings.HasPrefix(reason, "ExitCode:"), strings.HasPrefix(reason, "Init:ExitCode"):
		return reason, HealthBad
	case reason == "Running" && allReady:
		return reason, HealthOK
	case reason == "Running":
		return "Running (not ready)", HealthWarn
	default:
		return reason, HealthWarn
	}
}

func images(spec corev1.PodSpec) string {
	var out []string
	for _, c := range spec.Containers {
		img := c.Image
		if i := strings.LastIndex(img, "/"); i >= 0 {
			img = img[i+1:]
		}
		out = append(out, img)
	}
	return strings.Join(out, ", ")
}

func rolloutHealth(desired, available int32) (string, Health) {
	switch {
	case desired == 0:
		return "Scaled to 0", HealthNeutral
	case available >= desired:
		return "Available", HealthOK
	case available == 0:
		return "Unavailable", HealthBad
	default:
		return "Degraded", HealthWarn
	}
}

func listDeployments(c *cluster) []Resource {
	ds, _ := c.deploys.List(labels.Everything())
	out := make([]Resource, 0, len(ds))
	for _, d := range ds {
		want := ptrOr(d.Spec.Replicas, 1)
		status, h := rolloutHealth(want, d.Status.AvailableReplicas)
		if h == HealthOK && d.Status.UpdatedReplicas < want {
			status, h = "Rolling out", HealthWarn
		}
		if cond := deployCond(d, appsv1.DeploymentProgressing); cond != nil && cond.Reason == "ProgressDeadlineExceeded" {
			status, h = "Rollout stuck", HealthBad
		}
		out = append(out, Resource{
			Namespace: d.Namespace, Name: d.Name, Status: status, Health: h, Created: d.CreationTimestamp.Time,
			Cols: []string{fmt.Sprintf("%d/%d", d.Status.ReadyReplicas, want), fmt.Sprint(d.Status.UpdatedReplicas),
				fmt.Sprint(d.Status.AvailableReplicas), images(d.Spec.Template.Spec)},
		})
	}
	return out
}

func deployCond(d *appsv1.Deployment, t appsv1.DeploymentConditionType) *appsv1.DeploymentCondition {
	for i := range d.Status.Conditions {
		if d.Status.Conditions[i].Type == t {
			return &d.Status.Conditions[i]
		}
	}
	return nil
}

func listStatefulSets(c *cluster) []Resource {
	ss, _ := c.sts.List(labels.Everything())
	out := make([]Resource, 0, len(ss))
	for _, s := range ss {
		want := ptrOr(s.Spec.Replicas, 1)
		status, h := rolloutHealth(want, s.Status.ReadyReplicas)
		out = append(out, Resource{
			Namespace: s.Namespace, Name: s.Name, Status: status, Health: h, Created: s.CreationTimestamp.Time,
			Cols: []string{fmt.Sprintf("%d/%d", s.Status.ReadyReplicas, want), images(s.Spec.Template.Spec)},
		})
	}
	return out
}

func listDaemonSets(c *cluster) []Resource {
	ds, _ := c.ds.List(labels.Everything())
	out := make([]Resource, 0, len(ds))
	for _, d := range ds {
		st := d.Status
		status, h := rolloutHealth(st.DesiredNumberScheduled, st.NumberAvailable)
		out = append(out, Resource{
			Namespace: d.Namespace, Name: d.Name, Status: status, Health: h, Created: d.CreationTimestamp.Time,
			Cols: []string{fmt.Sprint(st.DesiredNumberScheduled), fmt.Sprint(st.NumberReady), fmt.Sprint(st.UpdatedNumberScheduled), fmt.Sprint(st.NumberAvailable)},
		})
	}
	return out
}

func listServices(c *cluster) []Resource {
	svcs, _ := c.services.List(labels.Everything())
	out := make([]Resource, 0, len(svcs))
	for _, s := range svcs {
		var ext []string
		for _, in := range s.Status.LoadBalancer.Ingress {
			ext = append(ext, firstNonEmpty(in.IP, in.Hostname))
		}
		ext = append(ext, s.Spec.ExternalIPs...)
		status, h := "Active", HealthOK
		if s.Spec.Type == corev1.ServiceTypeLoadBalancer && len(s.Status.LoadBalancer.Ingress) == 0 {
			ext = append(ext, "<pending>")
			status, h = "Waiting for IP", HealthWarn
		}
		if s.Spec.Type == corev1.ServiceTypeExternalName {
			ext = append(ext, s.Spec.ExternalName)
		}
		var ports []string
		for _, p := range s.Spec.Ports {
			if p.NodePort != 0 {
				ports = append(ports, fmt.Sprintf("%d:%d/%s", p.Port, p.NodePort, p.Protocol))
			} else {
				ports = append(ports, fmt.Sprintf("%d/%s", p.Port, p.Protocol))
			}
		}
		out = append(out, Resource{
			Namespace: s.Namespace, Name: s.Name, Status: status, Health: h, Created: s.CreationTimestamp.Time,
			Cols: []string{string(s.Spec.Type), s.Spec.ClusterIP, strings.Join(ext, ", "), strings.Join(ports, ", ")},
		})
	}
	return out
}

func listPVCs(c *cluster) []Resource {
	pvcs, _ := c.pvcs.List(labels.Everything())
	out := make([]Resource, 0, len(pvcs))
	for _, p := range pvcs {
		h := HealthOK
		switch p.Status.Phase {
		case corev1.ClaimPending:
			h = HealthWarn
		case corev1.ClaimLost:
			h = HealthBad
		}
		capacity := ""
		if q, ok := p.Status.Capacity[corev1.ResourceStorage]; ok {
			capacity = q.String()
		}
		sc := ""
		if p.Spec.StorageClassName != nil {
			sc = *p.Spec.StorageClassName
		}
		out = append(out, Resource{
			Namespace: p.Namespace, Name: p.Name, Status: string(p.Status.Phase), Health: h, Created: p.CreationTimestamp.Time,
			Cols: []string{p.Spec.VolumeName, capacity, sc},
		})
	}
	return out
}

func listNodes(c *cluster) []Resource {
	nodes, _ := c.nodes.List(labels.Everything())
	out := make([]Resource, 0, len(nodes))
	for _, n := range nodes {
		status, h := "NotReady", HealthBad
		if cond := nodeCondition(n, corev1.NodeReady); cond != nil && cond.Status == corev1.ConditionTrue {
			status, h = "Ready", HealthOK
		}
		for _, t := range pressureConditions {
			if cond := nodeCondition(n, t); cond != nil && cond.Status == corev1.ConditionTrue {
				status += ", " + string(t)
				h = HealthWarn
			}
		}
		if n.Spec.Unschedulable {
			status += ", SchedulingDisabled"
			if h == HealthOK {
				h = HealthWarn
			}
		}
		var roles []string
		for l := range n.Labels {
			if r, ok := strings.CutPrefix(l, "node-role.kubernetes.io/"); ok {
				roles = append(roles, r)
			}
		}
		slices.Sort(roles)
		out = append(out, Resource{
			Name: n.Name, Status: status, Health: h, Created: n.CreationTimestamp.Time,
			Cols: []string{strings.Join(roles, ","), poolOf(n), n.Status.NodeInfo.KubeletVersion, n.Status.NodeInfo.OSImage},
		})
	}
	return out
}

func eventRow(e *corev1.Event) Resource {
	count := e.Count
	if e.Series != nil {
		count = e.Series.Count
	}
	return Resource{
		Namespace: e.Namespace, Name: e.Name, Status: e.Reason, Health: HealthWarn, Created: eventTime(e),
		Cols: []string{strings.ToLower(e.InvolvedObject.Kind) + "/" + e.InvolvedObject.Name, truncate(e.Message, 300), fmt.Sprint(max(count, 1)), ago(eventTime(e))},
	}
}

func listEvents(c *cluster) []Resource {
	evs, _ := c.events.List(labels.Everything())
	out := make([]Resource, 0, len(evs))
	for _, e := range evs {
		out = append(out, eventRow(e))
	}
	return out
}

func ptrOr(p *int32, def int32) int32 {
	if p == nil {
		return def
	}
	return *p
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}
