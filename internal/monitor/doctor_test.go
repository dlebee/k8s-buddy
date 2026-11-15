package monitor

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
)

func TestIdleLoadBalancerMessage(t *testing.T) {
	s := &corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}}
	if m := idleLBMessage(s, "x"); !strings.Contains(m, "still requesting an external IP") {
		t.Fatalf("pending LB: %s", m)
	}
	s.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "20.1.2.3"}}
	if m := idleLBMessage(s, "selector app=x matches no running pods"); !strings.Contains(m, "20.1.2.3") || !strings.Contains(m, "billed") {
		t.Fatalf("allocated LB: %s", m)
	}
	ready := []discoveryv1.EndpointSlice{{}, {Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}}}}}
	if hasEndpoints(ready[:1]) || !hasEndpoints(ready) {
		t.Fatal("hasEndpoints")
	}
}

func TestNodePorts(t *testing.T) {
	s := &corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Ports: []corev1.ServicePort{{Port: 80, NodePort: 30080}, {Port: 443, NodePort: 30443}}}}
	if got := nodePorts(s); got != "30080, 30443" {
		t.Fatalf("got %q", got)
	}
}
