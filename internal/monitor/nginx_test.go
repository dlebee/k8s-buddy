package monitor

import (
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dlebee/k8s-buddy/internal/alert"
)

func ing(ns, name, typ, host string, age time.Duration, paths int, tls bool) *networkingv1.Ingress {
	class := "nginx-oss"
	i := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(time.Now().Add(-age)), Annotations: map[string]string{}},
		Spec:       networkingv1.IngressSpec{IngressClassName: &class},
	}
	if typ != "" {
		i.Annotations[mergeableAnnotation] = typ
	}
	rule := networkingv1.IngressRule{Host: host}
	if paths > 0 {
		rule.HTTP = &networkingv1.HTTPIngressRuleValue{}
		for range paths {
			rule.HTTP.Paths = append(rule.HTTP.Paths, networkingv1.HTTPIngressPath{Path: "/"})
		}
	}
	i.Spec.Rules = []networkingv1.IngressRule{rule}
	if tls {
		i.Spec.TLS = []networkingv1.IngressTLS{{Hosts: []string{host}}}
	}
	return i
}

func TestNginxMergeIssues(t *testing.T) {
	day := 24 * time.Hour
	got := nginxMergeIssuesT([]*networkingv1.Ingress{
		// healthy master + minion
		ing("a", "api-master", "master", "api.example.com", 10*day, 0, true),
		ing("a", "api-users", "minion", "api.example.com", 9*day, 2, false),
		// master shadowed by an older plain ingress for the same host (the cca-indexer case)
		ing("cca", "cca-indexer-plain", "", "cca.example.com", 120*day, 1, true),
		ing("dex", "cca-indexer-master", "master", "cca.example.com", 80*day, 0, true),
		// orphan minion
		ing("dryrun", "router", "minion", "dryrun.example.com", 18*day, 2, false),
		// master with paths (invalid) and a minion carrying TLS
		ing("b", "bad-master", "master", "b.example.com", 5*day, 1, true),
		ing("b", "tls-minion", "minion", "b.example.com", 4*day, 1, true),
		// plain ingresses only: not our business
		ing("c", "one", "", "c.example.com", 3*day, 1, false),
		ing("c", "two", "", "c.example.com", 2*day, 1, false),
	})
	want := map[string]struct {
		sev  alert.Severity
		text string
	}{
		"dex/cca-indexer-master": {alert.Warning, "also claimed by cca/cca-indexer-plain"},
		"dryrun/router":          {alert.Warning, "has no master"},
		"b/bad-master":           {alert.Warning, "can't define paths"},
		"b/tls-minion":           {alert.Notice, "TLS on a minion"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d issues: %+v", len(got), got)
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok || g.Severity != w.sev || !strings.Contains(g.Message, w.text) {
			t.Errorf("%s: got %+v, want %s containing %q", k, g, w.sev, w.text)
		}
	}

	// Empty master that wins its host gets a notice.
	got = nginxMergeIssuesT([]*networkingv1.Ingress{ing("x", "lonely", "master", "x.example.com", day, 0, true)})
	if g := got["x/lonely"]; g.Severity != alert.Notice || !strings.Contains(g.Message, "no minions") {
		t.Errorf("lonely master: %+v", g)
	}
}

func nginxMergeIssuesT(ings []*networkingv1.Ingress) map[string]mergeIssue {
	return nginxMergeIssues(ings, classInfo{})
}

func withAddress(i *networkingv1.Ingress) *networkingv1.Ingress {
	i.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "4.249.60.143"}}
	return i
}

func TestNginxIncSameHostPlainIngresses(t *testing.T) {
	ci := classesFrom([]*networkingv1.IngressClass{{
		ObjectMeta: metav1.ObjectMeta{Name: "nginx-oss"}, Spec: networkingv1.IngressClassSpec{Controller: nginxIncController},
	}})
	front := withAddress(ing("asc", "frontend", "", "explorer.example.com", time.Hour, 1, true))
	metrics := ing("asc", "frontend-metrics", "", "explorer.example.com", time.Hour, 1, true)
	metrics.Annotations["nginx.ingress.kubernetes.io/whitelist-source-range"] = "10.0.0.0/8"
	metrics.Annotations["meta.helm.sh/release-name"] = "blockscout"
	got := nginxMergeIssues([]*networkingv1.Ingress{front, metrics}, ci)
	g := got["asc/frontend-metrics"]
	for _, want := range []string{"already served by asc/frontend", "/ is answered by frontend's / route", "its settings don't apply (whitelist-source-range)"} {
		if !strings.Contains(g.Message, want) {
			t.Errorf("message %q missing %q", g.Message, want)
		}
	}
	if _, ok := got["asc/frontend"]; ok {
		t.Fatal("served ingress must not be flagged")
	}
	// The community controller merges same-host ingresses: nothing to report.
	ci.controllers["nginx-oss"] = communityController
	if got := nginxMergeIssues([]*networkingv1.Ingress{front, metrics}, ci); len(got) != 0 {
		t.Fatalf("community controller: %+v", got)
	}
}

func TestForeignAnnotations(t *testing.T) {
	i := ing("asc", "metrics", "", "x.example.com", time.Hour, 1, false)
	i.Annotations["nginx.ingress.kubernetes.io/whitelist-source-range"] = "10.0.0.0/8"
	i.Annotations["nginx.org/proxy-read-timeout"] = "3600"
	keys, sec := foreignAnnotations(i, nginxIncController)
	if len(keys) != 1 || keys[0] != "whitelist-source-range" || !sec {
		t.Fatalf("nginx.org controller: %v %v", keys, sec)
	}
	keys, sec = foreignAnnotations(i, communityController)
	if len(keys) != 1 || keys[0] != "proxy-read-timeout" || sec {
		t.Fatalf("community controller: %v %v", keys, sec)
	}
	if keys, _ := foreignAnnotations(i, "traefik.io/ingress-controller"); len(keys) != 0 {
		t.Fatalf("other controllers: %v", keys)
	}
}

func TestFallthroughLongestPrefix(t *testing.T) {
	served := []RouteIngress{{Namespace: "a", Name: "web", Paths: []RoutePath{{Path: "/", Service: "front"}, {Path: "/api", Service: "api"}}}}
	cases := map[string]string{"/node-api/metrics": "/", "/api/v1": "/api", "/api": "/api", "/apix": "/"}
	for in, want := range cases {
		if hop := fallthroughFor(in, served); hop == nil || hop.Path != want {
			t.Errorf("%s: got %+v want %s", in, hop, want)
		}
	}
	if hop := fallthroughFor("/x", []RouteIngress{{Paths: []RoutePath{{Path: "/api"}}}}); hop != nil {
		t.Errorf("no match expected, got %+v", hop)
	}
}
