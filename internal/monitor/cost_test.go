package monitor

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestPack(t *testing.T) {
	// Three 600m pods on nodes with 1000m of room: sums to 1.8 nodes but needs 3 (they don't pair).
	items := []packItem{{cpu: 600, mem: 1}, {cpu: 600, mem: 1}, {cpu: 600, mem: 1}}
	if n, why := pack(items, 1000, 100, false); n != 3 || why != "cpu" {
		t.Fatalf("got %d %s", n, why)
	}
	// By usage they are small: everything fits on one node.
	for i := range items {
		items[i].ucpu, items[i].umem = 100, 1
	}
	if n, _ := pack(items, 1000, 100, true); n != 1 {
		t.Fatalf("by usage got %d", n)
	}
	// Memory-bound.
	if n, why := pack([]packItem{{cpu: 1, mem: 60}, {cpu: 1, mem: 60}}, 1000, 100, false); n != 2 || why != "memory" {
		t.Fatalf("memory got %d %s", n, why)
	}
}

func TestConsolidate(t *testing.T) {
	into := CostCluster{Cluster: "b", Pools: []CostPool{{Nodes: 2, NodeCPU: 4000, NodeMem: 16 << 30, ReqCPU: 1000, ReqMem: 2 << 30}}}
	from := CostCluster{Cluster: "a", Monthly: 300, Priced: true}
	fw := map[string]wlFootprint{
		"web/Deployment/api":    {cpu: 1000, mem: 2 << 30},
		"db/StatefulSet/pg":     {cpu: 500, mem: 4 << 30, volumes: true},
		"kube-system/DS/agent":  {cpu: 100, mem: 1 << 20, system: true},
		"shared/Deployment/ing": {cpu: 200, mem: 1 << 20},
	}
	iw := map[string]wlFootprint{"shared/Deployment/ing": {}}
	c := consolidate("prod", from, fw, into, iw)
	if c == nil || c.Workloads != 2 || c.Already != 1 || len(c.WithVolumes) != 1 || c.Savings != 300 {
		t.Fatalf("got %+v", c)
	}
	// Too big to fit.
	fw["big/Deployment/x"] = wlFootprint{cpu: 9000, mem: 1}
	if consolidate("prod", from, fw, into, iw) != nil {
		t.Fatal("should not fit")
	}
}

func TestPinnedPod(t *testing.T) {
	if pinnedPod(&corev1.Pod{Spec: corev1.PodSpec{NodeSelector: map[string]string{"kubernetes.io/os": "linux"}}}) {
		t.Fatal("os selector is not pinning")
	}
	if !pinnedPod(&corev1.Pod{Spec: corev1.PodSpec{NodeSelector: map[string]string{"agentpool": "gpu"}}}) {
		t.Fatal("pool selector pins")
	}
}
