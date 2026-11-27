package monitor

import (
	"strings"
	"testing"
)

// A system workload's suggestion is never a runnable command (its owner undoes manual changes),
// and one from only a few days of usage says so.
func TestOptCommandCautions(t *testing.T) {
	r := OptRow{Cluster: "a", Namespace: "kube-system", Kind: "DaemonSet", Workload: "kube-proxy", Container: "kube-proxy", System: true, Days: 30}
	for _, line := range strings.Split(optCommand(&r), "\n") {
		if !strings.HasPrefix(line, "#") {
			t.Fatalf("a system workload got a runnable line: %q", line)
		}
	}
	if !strings.Contains(optCommand(&r), "don't apply") {
		t.Fatal("no don't-apply note")
	}
	r = OptRow{Cluster: "a", Namespace: "shop", Kind: "Deployment", Workload: "api", Container: "api", Days: 5}
	cmd := optCommand(&r)
	if !strings.Contains(cmd, "only 5.0 days") || !strings.Contains(cmd, "\nkubectl") {
		t.Fatalf("few days: %q", cmd)
	}
	r.Days = 9
	if cmd := optCommand(&r); strings.Contains(cmd, "#") || !strings.HasPrefix(cmd, "kubectl") {
		t.Fatalf("enough days: %q", cmd)
	}
}
