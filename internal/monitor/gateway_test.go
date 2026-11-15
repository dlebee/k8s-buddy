package monitor

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func obj(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestGatewayExposure(t *testing.T) {
	gw := parseGateway(obj(t, `{"metadata":{"namespace":"infra","name":"public"},
		"spec":{"gatewayClassName":"eg","listeners":[{"name":"https","protocol":"HTTPS","port":443,"hostname":"*.example.com"},{"name":"http","protocol":"HTTP","port":80}]},
		"status":{"addresses":[{"type":"IPAddress","value":"9.9.9.9"}],"conditions":[{"type":"Accepted","status":"True"},{"type":"Programmed","status":"True"}]}}`))
	broken := parseGateway(obj(t, `{"metadata":{"namespace":"infra","name":"pending"},"spec":{"gatewayClassName":"eg","listeners":[{"name":"http","protocol":"HTTP","port":80}]},
		"status":{"conditions":[{"type":"Accepted","status":"True"},{"type":"Programmed","status":"False","reason":"AddressNotAssigned","message":"no address yet"}]}}`))
	if gw.Problem != "" || broken.Problem == "" {
		t.Fatalf("problems: %q / %q", gw.Problem, broken.Problem)
	}
	route := parseRoute("HTTPRoute", obj(t, `{"metadata":{"namespace":"web","name":"api"},
		"spec":{"parentRefs":[{"name":"public","namespace":"infra","sectionName":"https"},{"name":"nope"}],"hostnames":["api.example.com"],
		  "rules":[{"matches":[{"path":{"type":"PathPrefix","value":"/v1"}}],"backendRefs":[{"name":"api","port":80}]},
		           {"backendRefs":[{"name":"gone","port":80},{"name":"shared","namespace":"other","port":80}]}]},
		"status":{"parents":[{"parentRef":{"name":"nope"},"conditions":[{"type":"Accepted","status":"False","reason":"NoMatchingParent"}]}]}}`))
	grpc := parseRoute("GRPCRoute", obj(t, `{"metadata":{"namespace":"web","name":"rpc"},
		"spec":{"parentRefs":[{"name":"public","namespace":"infra"}],"rules":[{"matches":[{"method":{"service":"pkg.Echo","method":"Say"}}],"backendRefs":[{"name":"api","port":80}]}]}}`))

	pod := func(ns, name string, lbl map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: lbl},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	}
	pods := []*corev1.Pod{pod("envoy-gateway-system", "envoy-1", map[string]string{"app": "envoy"}), pod("web", "api-1", map[string]string{"app": "api"})}
	envoySvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "envoy-gateway-system", Name: "envoy-infra-public-abc",
		Labels: map[string]string{"gateway.envoyproxy.io/owning-gateway-name": "public", "gateway.envoyproxy.io/owning-gateway-namespace": "infra"}},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Selector: map[string]string{"app": "envoy"}}}
	apiSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "web", Name: "api"},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "api"}, Ports: []corev1.ServicePort{{Port: 80}}}}

	entries := buildExposure(pods, []*corev1.Service{envoySvc, apiSvc}, nil, classInfo{},
		gatewayData{Gateways: []gwGateway{gw, broken}, Routes: []gwRoute{route, grpc}, Classes: map[string]string{"eg": "gateway.envoyproxy.io/gatewayclass-controller"}})

	byName := map[string]ExposureEntry{}
	for _, e := range entries {
		if e.Kind == "lb" {
			t.Errorf("gateway's service also listed as a plain LoadBalancer: %s", e.Name)
		}
		byName[e.Name] = e
	}
	pub := byName["public"]
	if pub.Kind != "gateway" || pub.Controller != "gateway.envoyproxy.io/gatewayclass-controller" || len(pub.Addresses) != 1 || pub.Addresses[0] != "9.9.9.9" {
		t.Fatalf("public gateway: %+v", pub)
	}
	if len(pub.Workloads) != 1 || pub.Workloads[0].Total != 1 {
		t.Errorf("gateway pods: %+v", pub.Workloads)
	}
	if len(pub.Ingresses) != 2 {
		t.Fatalf("routes on public: %+v", pub.Ingresses)
	}
	r := pub.Ingresses[0]
	if r.Kind != "HTTPRoute" || !r.TLS || r.Problem != "" || len(r.Services) != 3 {
		t.Fatalf("httproute: %+v", r)
	}
	if got := r.Services[0].Routes; len(got) != 1 || got[0] != "api.example.com/v1" {
		t.Errorf("api routes: %v", got)
	}
	if !r.Services[1].Missing || r.Services[2].Namespace != "other" {
		t.Errorf("backends: %+v", r.Services)
	}
	if g := pub.Ingresses[1]; g.Kind != "GRPCRoute" || !g.TLS || g.Hosts[0] != "*.example.com" || g.Services[0].Routes[0] != "*.example.com/pkg.Echo/Say" {
		t.Errorf("grpcroute: %+v", g)
	}
	if byName["pending"].Problem == "" {
		t.Error("pending gateway has no problem")
	}
	miss := byName["missing gateway web/nope"]
	if miss.Kind != "class" || len(miss.Ingresses) != 1 || miss.Ingresses[0].Problem == "" {
		t.Errorf("missing parent: %+v", miss)
	}
}

func TestGatewayOfService(t *testing.T) {
	for _, tc := range []struct {
		labels map[string]string
		want   string
	}{
		{map[string]string{"gateway.networking.k8s.io/gateway-name": "gw"}, "ns/gw"},
		{map[string]string{"gateway.envoyproxy.io/owning-gateway-name": "gw", "gateway.envoyproxy.io/owning-gateway-namespace": "apps"}, "apps/gw"},
		{map[string]string{"app": "x"}, ""},
	} {
		if got := gatewayOfService(tc.labels, "ns"); got != tc.want {
			t.Errorf("%v: got %q want %q", tc.labels, got, tc.want)
		}
	}
}
