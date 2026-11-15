package monitor

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIngressURL(t *testing.T) {
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Spec: networkingv1.IngressSpec{
			TLS: []networkingv1.IngressTLS{{Hosts: []string{"secure.example.com"}}},
			Rules: []networkingv1.IngressRule{
				{Host: "secure.example.com", IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Path: "/api(/|$)(.*)"}, {Path: "/docs"}}}}},
				{Host: "plain.example.com", IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Path: "/"}}}}},
				{Host: "*.wild.example.com"},
			},
		},
	}
	for _, tc := range []struct{ host, path, want string }{
		{"secure.example.com", "/docs", "https://secure.example.com/docs"},
		{"secure.example.com", "/api(/|$)(.*)", "https://secure.example.com/api"},
		{"secure.example.com", "", "https://secure.example.com/"},
		{"plain.example.com", "/", "http://plain.example.com/"},
		{"plain.example.com", "/other", ""},
		{"evil.example.com", "/", ""},
		{"*.wild.example.com", "", ""},
		{"secure.example.com@evil.com", "", ""},
	} {
		got, err := ingressURL(ing, tc.host, tc.path)
		if tc.want == "" {
			if err == nil {
				t.Errorf("%s%s: want error, got %s", tc.host, tc.path, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s%s: got %q, %v; want %q", tc.host, tc.path, got, err, tc.want)
		}
	}
}
