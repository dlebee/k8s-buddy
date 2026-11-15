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
	got := nginxMergeIssues([]*networkingv1.Ingress{
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
	got = nginxMergeIssues([]*networkingv1.Ingress{ing("x", "lonely", "master", "x.example.com", day, 0, true)})
	if g := got["x/lonely"]; g.Severity != alert.Notice || !strings.Contains(g.Message, "no minions") {
		t.Errorf("lonely master: %+v", g)
	}
}
