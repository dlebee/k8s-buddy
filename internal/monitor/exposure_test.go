package monitor

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildExposure(t *testing.T) {
	pod := func(ns, name, rs, hash, image string, lbl map[string]string, ready bool) *corev1.Pod {
		l := map[string]string{"pod-template-hash": hash}
		for k, v := range lbl {
			l[k] = v
		}
		st := corev1.ConditionFalse
		if ready {
			st = corev1.ConditionTrue
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: l, OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: rs}}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Image: image}}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}},
		}
	}
	svc := func(ns, name string, typ corev1.ServiceType, sel map[string]string, ip string) *corev1.Service {
		s := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: corev1.ServiceSpec{Type: typ, Selector: sel, Ports: []corev1.ServicePort{{Port: 80, NodePort: 30080, Protocol: corev1.ProtocolTCP}}}}
		if ip != "" {
			s.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: ip}}
		}
		return s
	}
	pods := []*corev1.Pod{
		pod("ingress", "ctrl-abc-1", "ctrl-abc", "abc", "registry.k8s.io/ingress-nginx/controller:v1", map[string]string{"app": "ctrl"}, true),
		pod("web", "api-77-1", "api-77", "77", "api", map[string]string{"app": "api"}, true),
		pod("web", "api-77-2", "api-77", "77", "api", map[string]string{"app": "api"}, true),
		pod("web", "api-77-3", "api-77", "77", "api", map[string]string{"app": "api"}, false),
		pod("rpc", "node-1", "node-9", "9", "node", map[string]string{"app": "node"}, true),
	}
	svcs := []*corev1.Service{
		svc("ingress", "ctrl", corev1.ServiceTypeLoadBalancer, map[string]string{"app": "ctrl"}, "1.2.3.4"),
		svc("web", "api", corev1.ServiceTypeClusterIP, map[string]string{"app": "api"}, ""),
		svc("rpc", "node-lb", corev1.ServiceTypeLoadBalancer, map[string]string{"app": "node"}, "5.6.7.8"),
		svc("rpc", "node-np", corev1.ServiceTypeNodePort, map[string]string{"app": "node"}, ""),
	}
	pt := networkingv1.PathTypePrefix
	ings := []*networkingv1.Ingress{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "web", Name: "api"},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: "api.example.com", IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{
			{Path: "/", PathType: &pt, Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "api"}}},
			{Path: "/v2", PathType: &pt, Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "api"}}},
			{Path: "/gone", PathType: &pt, Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "gone"}}},
		}}}}}},
	}}

	got := buildExposure(pods, svcs, ings, classInfo{}, gatewayData{})
	if len(got) != 3 || got[0].Kind != "controller" || got[1].Kind != "lb" || got[2].Kind != "nodeport" {
		t.Fatalf("entries: %+v", got)
	}
	ctrl := got[0]
	if ctrl.Controller != "ingress-nginx" || ctrl.Addresses[0] != "1.2.3.4" || len(ctrl.Ingresses) != 1 {
		t.Fatalf("controller: %+v", ctrl)
	}
	ing := ctrl.Ingresses[0]
	if len(ing.Services) != 2 || len(ing.Services[0].Routes) != 2 || !ing.Services[1].Missing {
		t.Fatalf("ingress services: %+v", ing.Services)
	}
	w := ing.Services[0].Workloads
	if len(w) != 1 || w[0].Name != "api" || w[0].Kind != "Deployment" || w[0].Total != 3 || w[0].Ready != 2 {
		t.Fatalf("workloads: %+v", w)
	}
	if len(w[0].Pods) != 3 || w[0].Pods[2].Ready || w[0].Pods[0].Name != "api-77-1" {
		t.Fatalf("pods: %+v", w[0].Pods)
	}
	if got[1].Workloads[0].Name != "node" || got[2].Ports[0] != "80:30080/TCP" {
		t.Fatalf("direct entries: %+v %+v", got[1], got[2])
	}

	// No exposed controller: ingresses gather under their class.
	got = buildExposure(pods, svcs[1:], ings, classInfo{controllers: map[string]string{"nginx": communityController}, def: "nginx"}, gatewayData{})
	if got[0].Kind != "class" || got[0].Name != "nginx" || len(got[0].Ingresses) != 1 {
		t.Fatalf("class entry: %+v", got[0])
	}
}
