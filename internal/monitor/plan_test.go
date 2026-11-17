package monitor

import "testing"

func TestShrinkPlanRespectsDiskLimits(t *testing.T) {
	mk := func(name string, disks int) *planNode {
		return &planNode{name: name, pool: "p", allocCPU: 4000, allocMem: 16 << 30, diskLimit: 4, disks: disks}
	}
	a, b, c := mk("a", 1), mk("b", 3), mk("c", 3)
	b.usedCPU, b.usedMem, c.usedCPU, c.usedMem = 1500, 4<<30, 1500, 4<<30 // busier than a
	db := &planPod{ns: "data", name: "pg-0", kind: "StatefulSet", workload: "pg", node: "a", cpu: 500, mem: 1 << 30, disks: []string{"pvc-1"}}
	web := &planPod{ns: "web", name: "api-x", kind: "Deployment", workload: "api", node: "a", cpu: 500, mem: 1 << 30}
	a.pods = []*planPod{db, web}
	a.usedCPU, a.usedMem = 1000, 2<<30
	all := []*planPod{db, web}

	// b and c have one free disk slot each: pg-0's disk fits, so node a can be drained.
	plan, ok := shrinkPlan("c1", []*planNode{a, b, c}, 1, all)
	if !ok || plan.Drain[0].Name != "a" || plan.NodesTo != 2 || len(plan.Moves) != 2 {
		t.Fatalf("plan: %+v ok=%v", plan, ok)
	}
	var disks int
	for _, n := range plan.After {
		disks += n.Disks
	}
	if disks != 7 {
		t.Fatalf("disks after = %d, want 7 (3+3 plus the moved one)", disks)
	}

	// Full disk slots everywhere else: the plan must not be offered.
	b.disks, c.disks = 4, 4
	if _, ok := shrinkPlan("c1", []*planNode{a, b, c}, 1, all); ok {
		t.Fatal("pod with a disk placed on nodes at their attach limit")
	}

	// A node with a pinned pod is never chosen for draining.
	b.disks, c.disks = 1, 1
	web.pinned = true
	plan, ok = shrinkPlan("c1", []*planNode{a, b, c}, 1, all)
	if !ok || plan.Drain[0].Name == "a" {
		t.Fatalf("drained the node holding a pinned pod: %+v", plan)
	}
}

func TestCompactCounts(t *testing.T) {
	got := compactCounts([]string{"b", "a", "b"})
	if len(got) != 2 || got[0] != "a" || got[1] != "b ×2" {
		t.Fatalf("got %v", got)
	}
}
