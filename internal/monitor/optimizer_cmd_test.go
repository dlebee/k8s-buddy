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

// An ok row keeps what's set: it never reads as advice to cut a limit that only has room to spare.
func TestOkRowSuggestsNoChange(t *testing.T) {
	r := OptRow{Cluster: "a", Namespace: "shop", Kind: "Deployment", Workload: "api", Container: "api", Replicas: 2, Days: 10,
		CPU: OptResource{Median: 20, P95: 30, Max: 40, Request: OptValue{50, 25}, Limit: OptValue{1000, 50}},
		Mem: OptResource{Median: 100 << 20, P95: 110 << 20, Max: 120 << 20, Request: OptValue{128 << 20, 120 << 20}, Limit: OptValue{256 << 20, 160 << 20}}}
	judge(&r)
	if r.Verdict != "ok" {
		t.Fatalf("verdict %s, findings %v", r.Verdict, r.Findings)
	}
	if r.CPU.Limit.Recommended != 1000 || r.CPU.Request.Recommended != 50 || r.Mem.Limit.Recommended != 256<<20 || r.FreeCPU != 0 || r.FreeMem != 0 {
		t.Fatalf("an ok row still suggests changes: cpu %+v mem %+v free %d/%d", r.CPU, r.Mem, r.FreeCPU, r.FreeMem)
	}
	if !strings.HasPrefix(r.Command, "# ok") || strings.Contains(r.Command, "kubectl") {
		t.Fatalf("command: %q", r.Command)
	}
	// Still ok, but no CPU limit set: that one value keeps its suggestion.
	r.CPU.Limit = OptValue{-1, 50}
	judge(&r)
	if r.Verdict != "ok" || r.CPU.Limit.Recommended != 50 || !strings.Contains(r.Command, "kubectl") {
		t.Fatalf("unset value: verdict %s cpu %+v command %q", r.Verdict, r.CPU, r.Command)
	}
}
