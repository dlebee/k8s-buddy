package server

import (
	"strings"
	"testing"
)

func TestShQuote(t *testing.T) {
	cases := map[string]string{
		"prod-1":                "prod-1",
		"arn:aws:eks:x/cluster": "arn:aws:eks:x/cluster",
		"has space":             "'has space'",
		"it's":                  `'it'\''s'`,
		"$(rm -rf ~)":           "'$(rm -rf ~)'",
		"":                      "''",
	}
	for in, want := range cases {
		if got := shQuote(in); got != want {
			t.Errorf("shQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShellCommand(t *testing.T) {
	got := shellCommand(shellRequest{Cluster: "k3s", Namespace: "demo", Pod: "web-1", Container: "app"})
	want := "kubectl --context k3s -n demo exec -it web-1 -c app -- sh -c 'command -v bash >/dev/null 2>&1 && exec bash || exec sh'"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	script := shellScript(shellRequest{Cluster: "c", Namespace: "n", Pod: "p", Container: "x'; rm -rf ~; '"}, "/tmp/my config")
	if !strings.Contains(script, `-c 'x'\''; rm -rf ~; '\'''`) || !strings.Contains(script, "export KUBECONFIG='/tmp/my config'") {
		t.Fatalf("script not quoted safely:\n%s", script)
	}
}

func TestValidName(t *testing.T) {
	for _, s := range []string{"", "-n", "a/b", "a b", "--all"} {
		if validName(s) {
			t.Errorf("validName(%q) = true", s)
		}
	}
	if !validName("web-549cd57bfd-79zjg") {
		t.Error("pod name rejected")
	}
}

func TestForwardRejectsBadRequests(t *testing.T) {
	f := &forwarder{env: func() []string { return nil }}
	for _, req := range []forwardRequest{
		{Cluster: "c", Namespace: "n", Kind: "secrets", Name: "x", Port: 80},
		{Cluster: "c", Namespace: "n", Kind: "pods", Name: "--address=0.0.0.0", Port: 80},
		{Cluster: "c", Namespace: "n", Kind: "pods", Name: "x", Port: 70000},
		{Cluster: "", Namespace: "n", Kind: "pods", Name: "x", Port: 80},
	} {
		if _, err := f.start(req); err == nil {
			t.Errorf("start(%+v) succeeded", req)
		}
	}
}
