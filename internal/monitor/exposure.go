package monitor

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// The exposure map answers "what can reach this cluster from outside, and where does it land?":
// entry points (ingress controllers, LoadBalancer and NodePort services) → ingresses → services → workloads.

type ExposureWorkload struct {
	Kind  string        `json:"kind"`
	Name  string        `json:"name"`
	Total int           `json:"total"`
	Ready int           `json:"ready"`
	Pods  []ExposurePod `json:"pods"`
}

type ExposurePod struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Health   Health `json:"health"`
	Ready    bool   `json:"ready"`
	Restarts int32  `json:"restarts"`
	Node     string `json:"node,omitempty"`
	IP       string `json:"ip,omitempty"`
}

type ExposureService struct {
	Namespace string             `json:"namespace"`
	Name      string             `json:"name"`
	Type      string             `json:"type"`
	Ports     []string           `json:"ports"`
	Missing   bool               `json:"missing,omitempty"`
	Routes    []string           `json:"routes"`
	Workloads []ExposureWorkload `json:"workloads"`
}

type ExposureIngress struct {
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Class     string            `json:"class,omitempty"`
	TLS       bool              `json:"tls"`
	Hosts     []string          `json:"hosts"`
	Services  []ExposureService `json:"services"`
}

// Entry kinds: "controller" (an exposed ingress controller), "lb" and "nodeport" (services exposed
// straight to their pods), and "class" (ingresses whose controller isn't exposed by a service here).
type ExposureEntry struct {
	Kind       string             `json:"kind"`
	Namespace  string             `json:"namespace"`
	Name       string             `json:"name"`
	Type       string             `json:"type"`
	Ports      []string           `json:"ports"`
	Addresses  []string           `json:"addresses"`
	Internal   bool               `json:"internal,omitempty"`
	Controller string             `json:"controller,omitempty"`
	Workloads  []ExposureWorkload `json:"workloads"`
	Ingresses  []ExposureIngress  `json:"ingresses"`
}

type ClusterExposure struct {
	Cluster  string          `json:"cluster"`
	Status   string          `json:"status"` // ok | loading | error
	Error    string          `json:"error,omitempty"`
	Pods     int             `json:"pods"`
	Services int             `json:"services"`
	Entries  []ExposureEntry `json:"entries"`
}

// Exposure builds the map for every connected cluster, or just the named ones.
func (m *Manager) Exposure(only []string) []ClusterExposure {
	var list []*cluster
	for _, c := range m.connected() {
		if len(only) == 0 || slices.Contains(only, c.name) {
			list = append(list, c)
		}
	}
	out := make([]ClusterExposure, len(list))
	var wg sync.WaitGroup
	for i, c := range list {
		wg.Go(func() { out[i] = c.exposure() })
	}
	wg.Wait()
	return out
}

func (c *cluster) exposure() ClusterExposure {
	ce := ClusterExposure{Cluster: c.name, Status: "ok", Entries: []ExposureEntry{}}
	if !c.synced("pods") || !c.synced("services") || !c.synced("ingresses") {
		c.mu.Lock()
		st, msg := c.status, c.lastErr
		c.mu.Unlock()
		ce.Status, ce.Error = "loading", msg
		if st == StatusError {
			ce.Status = "error"
		}
		return ce
	}
	pods, _ := c.pods.List(labels.Everything())
	svcs, _ := c.services.List(labels.Everything())
	ings, _ := c.ingresses.List(labels.Everything())
	ce.Pods, ce.Services = len(pods), len(svcs)
	ce.Entries = buildExposure(pods, svcs, ings, c.classInfo())
	return ce
}

// Known ingress controllers, matched against container images, and the IngressClass controller
// string each one registers (used to route ingresses when a cluster runs several).
var ingressControllers = []struct{ image, name, class string }{
	{"ingress-nginx", "ingress-nginx", "k8s.io/ingress-nginx"},
	{"nginx-ingress", "nginx-ingress", "nginx.org/ingress-controller"},
	{"traefik", "traefik", "traefik.io/ingress-controller"},
	{"haproxy", "haproxy", "haproxy"},
	{"contour", "contour", "projectcontour.io"},
	{"istio", "istio", "istio.io"},
	{"kong", "kong", "ingress-controllers.konghq.com"},
	{"emissary", "emissary", "getambassador.io"},
	{"ambassador", "ambassador", "getambassador.io"},
	{"skipper", "skipper", "zalando.org/skipper"},
	{"caddy", "caddy", "caddy"},
	{"application-gateway-kubernetes-ingress", "agic", "azure/application-gateway"},
}

func controllerOf(pods []*corev1.Pod) string {
	for _, p := range pods {
		for _, ct := range p.Spec.Containers {
			for _, ic := range ingressControllers {
				if strings.Contains(ct.Image, ic.image) {
					return ic.name
				}
			}
		}
	}
	return ""
}

var internalLBAnnotations = map[string]string{
	"service.beta.kubernetes.io/azure-load-balancer-internal": "true",
	"service.beta.kubernetes.io/aws-load-balancer-internal":   "true",
	"service.beta.kubernetes.io/aws-load-balancer-scheme":     "internal",
	"networking.gke.io/load-balancer-type":                    "Internal",
	"cloud.google.com/load-balancer-type":                     "Internal",
}

func isInternalLB(s *corev1.Service) bool {
	for k, v := range internalLBAnnotations {
		if strings.EqualFold(s.Annotations[k], v) {
			return true
		}
	}
	return false
}

func servicePorts(s *corev1.Service) []string {
	out := make([]string, 0, len(s.Spec.Ports))
	for _, p := range s.Spec.Ports {
		v := fmt.Sprint(p.Port)
		if p.NodePort != 0 {
			v += fmt.Sprintf(":%d", p.NodePort)
		}
		out = append(out, v+"/"+string(p.Protocol))
	}
	return out
}

func podIsReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cond := range p.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// workloadsOf groups pods by owning workload so a three-replica deployment shows once with a counter.
func workloadsOf(pods []*corev1.Pod) []ExposureWorkload {
	out := []ExposureWorkload{} // never null in JSON
	idx := map[string]int{}
	for _, p := range pods {
		kind, name := "Pod", p.Name
		if len(p.OwnerReferences) > 0 {
			o := p.OwnerReferences[0]
			kind, name = o.Kind, o.Name
			if o.Kind == "ReplicaSet" {
				kind = "Deployment"
				if h := p.Labels["pod-template-hash"]; h != "" {
					name = strings.TrimSuffix(name, "-"+h)
				}
			}
		}
		key := kind + "/" + name
		i, ok := idx[key]
		if !ok {
			i = len(out)
			idx[key] = i
			out = append(out, ExposureWorkload{Kind: kind, Name: name})
		}
		ready := podIsReady(p)
		status, health := podStatus(p)
		var restarts int32
		for _, cs := range p.Status.ContainerStatuses {
			restarts += cs.RestartCount
		}
		out[i].Total++
		if ready {
			out[i].Ready++
		}
		out[i].Pods = append(out[i].Pods, ExposurePod{Name: p.Name, Status: status, Health: health, Ready: ready, Restarts: restarts, Node: p.Spec.NodeName, IP: p.Status.PodIP})
	}
	return out
}

func buildExposure(pods []*corev1.Pod, svcs []*corev1.Service, ings []*networkingv1.Ingress, ci classInfo) []ExposureEntry {
	podsByNS := map[string][]*corev1.Pod{}
	for _, p := range pods {
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		podsByNS[p.Namespace] = append(podsByNS[p.Namespace], p)
	}
	selected := func(s *corev1.Service) []*corev1.Pod {
		if len(s.Spec.Selector) == 0 {
			return nil
		}
		sel := labels.SelectorFromSet(s.Spec.Selector)
		var out []*corev1.Pod
		for _, p := range podsByNS[s.Namespace] {
			if sel.Matches(labels.Set(p.Labels)) {
				out = append(out, p)
			}
		}
		return out
	}
	svcIndex := map[string]*corev1.Service{}
	for _, s := range svcs {
		svcIndex[s.Namespace+"/"+s.Name] = s
	}

	var entries []ExposureEntry
	var controllers []int
	for _, s := range svcs {
		if s.Spec.Type != corev1.ServiceTypeLoadBalancer && s.Spec.Type != corev1.ServiceTypeNodePort {
			continue
		}
		ps := selected(s)
		e := ExposureEntry{
			Kind: "nodeport", Namespace: s.Namespace, Name: s.Name, Type: string(s.Spec.Type),
			Ports: servicePorts(s), Addresses: []string{}, Internal: isInternalLB(s),
			Workloads: workloadsOf(ps), Ingresses: []ExposureIngress{},
		}
		if s.Spec.Type == corev1.ServiceTypeLoadBalancer {
			e.Kind = "lb"
		}
		for _, in := range s.Status.LoadBalancer.Ingress {
			e.Addresses = append(e.Addresses, firstNonEmpty(in.IP, in.Hostname))
		}
		e.Addresses = append(e.Addresses, s.Spec.ExternalIPs...)
		if ctrl := controllerOf(ps); ctrl != "" {
			e.Kind, e.Controller = "controller", ctrl
			controllers = append(controllers, len(entries))
		}
		entries = append(entries, e)
	}

	// owner picks the exposed controller that serves an ingress class; -1 when none does.
	owner := func(class string) int {
		if len(controllers) == 0 {
			return -1
		}
		if len(controllers) == 1 {
			return controllers[0]
		}
		want := ci.controllerFor(class)
		lc := strings.ToLower(firstNonEmpty(class, ci.def))
		for _, i := range controllers { // the service is named after its class (e.g. nginx-internal)
			if lc != "" && (strings.Contains(strings.ToLower(entries[i].Name), lc) || strings.EqualFold(entries[i].Namespace, lc)) {
				return i
			}
		}
		for _, i := range controllers { // else the controller software registered for the class
			for _, ic := range ingressControllers {
				if ic.name == entries[i].Controller && want != "" && strings.HasPrefix(want, ic.class) {
					return i
				}
			}
		}
		return controllers[0]
	}
	pseudo := map[string]int{}
	for _, ing := range ings {
		class := ingressClassOf(ing.Annotations, ing.Spec.IngressClassName)
		ei := ExposureIngress{Namespace: ing.Namespace, Name: ing.Name, Class: class, TLS: len(ing.Spec.TLS) > 0, Hosts: []string{}, Services: []ExposureService{}}
		byName := map[string]int{}
		addRoute := func(svc, route string) {
			if svc == "" {
				return
			}
			i, ok := byName[svc]
			if !ok {
				i = len(ei.Services)
				byName[svc] = i
				es := ExposureService{Namespace: ing.Namespace, Name: svc, Routes: []string{}, Ports: []string{}, Workloads: []ExposureWorkload{}}
				if s, found := svcIndex[ing.Namespace+"/"+svc]; found {
					es.Type, es.Ports, es.Workloads = string(s.Spec.Type), servicePorts(s), workloadsOf(selected(s))
					if es.Type == "" {
						es.Type = string(corev1.ServiceTypeClusterIP)
					}
				} else {
					es.Type, es.Missing = "missing", true
				}
				ei.Services = append(ei.Services, es)
			}
			if !slices.Contains(ei.Services[i].Routes, route) {
				ei.Services[i].Routes = append(ei.Services[i].Routes, route)
			}
		}
		if b := ing.Spec.DefaultBackend; b != nil && b.Service != nil {
			addRoute(b.Service.Name, "* (default backend)")
		}
		for _, r := range ing.Spec.Rules {
			host := firstNonEmpty(r.Host, "*")
			if !slices.Contains(ei.Hosts, host) {
				ei.Hosts = append(ei.Hosts, host)
			}
			if r.HTTP == nil {
				continue
			}
			for _, p := range r.HTTP.Paths {
				if p.Backend.Service != nil {
					addRoute(p.Backend.Service.Name, host+firstNonEmpty(p.Path, "/"))
				}
			}
		}
		i := owner(class)
		if i < 0 {
			key := firstNonEmpty(class, ci.def, "(no class)")
			j, ok := pseudo[key]
			if !ok {
				j = len(entries)
				pseudo[key] = j
				entries = append(entries, ExposureEntry{Kind: "class", Name: key, Type: "IngressClass", Controller: ci.controllerFor(class),
					Ports: []string{}, Addresses: []string{}, Workloads: []ExposureWorkload{}, Ingresses: []ExposureIngress{}})
			}
			i = j
		}
		entries[i].Ingresses = append(entries[i].Ingresses, ei)
	}

	rank := map[string]int{"controller": 0, "class": 1, "lb": 2, "nodeport": 3}
	slices.SortStableFunc(entries, func(a, b ExposureEntry) int {
		if d := rank[a.Kind] - rank[b.Kind]; d != 0 {
			return d
		}
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	for i := range entries {
		slices.SortFunc(entries[i].Ingresses, func(a, b ExposureIngress) int {
			return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
		})
	}
	return entries
}
