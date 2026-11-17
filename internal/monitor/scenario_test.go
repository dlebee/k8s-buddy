package monitor

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func node(cluster, name string, disks, diskLimit int) *planNode {
	n := &planNode{cluster: cluster, name: name, pool: "p", allocCPU: 4000, allocMem: 16 << 30, disks: disks, diskLimit: diskLimit}
	n.limCap = n.allocMem
	return n
}

func put(n *planNode, p *planPod) {
	p.cluster, p.node = n.cluster, n.name
	n.pods = append(n.pods, p)
	n.usedCPU += p.cpu
	n.usedMem += p.mem
	n.limMem += p.memLim
}

func TestSimulateRespectsDisksLimitsAndTaints(t *testing.T) {
	pool := CostPool{Name: "p", Nodes: 3, Priced: true, Hourly: 0.1}
	a, b, c := node("c1", "a", 1, 4), node("c1", "b", 3, 4), node("c1", "c", 3, 4)
	db := &planPod{ns: "data", name: "pg-0", kind: "StatefulSet", workload: "pg", cpu: 500, mem: 1 << 30, memLim: 2 << 30, disks: []string{"pvc-1"}}
	web := &planPod{ns: "web", name: "api-1", kind: "Deployment", workload: "api", cpu: 500, mem: 1 << 30, memLim: 1 << 30}
	put(a, db)
	put(a, web)
	total := map[string]int{"data/StatefulSet/pg": 1, "web/Deployment/api": 3}

	sc, ok := simulate("c1", pool, []*planNode{a, b, c}, []*planNode{a}, total, nil)
	if !ok || sc.NodesTo != 2 || sc.Replicas != 2 || sc.DisksMoved != 1 || sc.Outages != 1 || sc.Strategy != "in-cluster" {
		t.Fatalf("in-cluster: %+v ok=%v", sc, ok)
	}

	// Every other node at its disk limit: pg-0's disk has nowhere to attach.
	b.disks, c.disks = 4, 4
	if _, ok := simulate("c1", pool, []*planNode{a, b, c}, []*planNode{a}, total, nil); ok {
		t.Fatal("placed a disk on nodes at their attach limit")
	}
	b.disks, c.disks = 3, 3

	// Memory limits: targets already hold limits up to their allocatable memory.
	b.limMem, c.limMem = b.limCap, c.limCap
	if _, ok := simulate("c1", pool, []*planNode{a, b, c}, []*planNode{a}, total, nil); ok {
		t.Fatal("overcommitted memory limits")
	}
	b.limMem, c.limMem = 0, 0

	// A NoSchedule taint the pods don't tolerate keeps them off.
	b.taints = []corev1.Taint{{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule}}
	c.taints = b.taints
	if _, ok := simulate("c1", pool, []*planNode{a, b, c}, []*planNode{a}, total, nil); ok {
		t.Fatal("ignored a taint")
	}
	web.tolerations = []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "gpu", Effect: corev1.TaintEffectNoSchedule}}
	db.tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	if _, ok := simulate("c1", pool, []*planNode{a, b, c}, []*planNode{a}, total, nil); !ok {
		t.Fatal("tolerated taints should allow placement")
	}
	b.taints, c.taints = nil, nil

	// No room in the cluster: an app replica may spill to another studied cluster.
	b.usedCPU, c.usedCPU = 3400, 3400
	other := node("c2", "x", 0, 8)
	sc, ok = simulate("c1", pool, []*planNode{a, b, c}, []*planNode{a}, total, []*planNode{other})
	if !ok || sc.Strategy != "spill" || sc.CrossCluster != 2 || sc.DataMigration != 0 || sc.TargetsUsed[0] != "c2" {
		t.Fatalf("spill: %+v ok=%v", sc, ok)
	}
	db.pvcs = []PlanVolume{{Claim: "data-pg-0", Disk: true}}
	sc, _ = simulate("c1", pool, []*planNode{a, b, c}, []*planNode{a}, total, []*planNode{other})
	if sc.DataMigration != 1 {
		t.Fatalf("cross-cluster volume should need data migration: %+v", sc)
	}
}

func TestTolerates(t *testing.T) {
	taint := &corev1.Taint{Key: "k", Value: "v", Effect: corev1.TaintEffectNoSchedule}
	for _, tc := range []struct {
		tol  corev1.Toleration
		want bool
	}{
		{corev1.Toleration{Operator: corev1.TolerationOpExists}, true},
		{corev1.Toleration{Key: "k", Operator: corev1.TolerationOpExists}, true},
		{corev1.Toleration{Key: "k", Value: "v"}, true},
		{corev1.Toleration{Key: "k", Value: "x"}, false},
		{corev1.Toleration{Key: "k", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute}, false},
	} {
		if got := tolerates(tc.tol, taint); got != tc.want {
			t.Errorf("%+v: got %v", tc.tol, got)
		}
	}
}
