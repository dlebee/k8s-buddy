package monitor

import (
	"testing"

	"github.com/dlebee/k8s-buddy/internal/history"
	"k8s.io/apimachinery/pkg/labels"
)

func TestAssessConfidence(t *testing.T) {
	two := func() (CostPool, []*planNode) {
		a, b := node("c1", "a", 0, -1), node("c1", "b", 0, -1)
		put(a, &planPod{ns: "web", name: "api-1", kind: "Deployment", workload: "api", cpu: 500, mem: 1 << 30, labels: map[string]string{"app": "api"}})
		put(b, &planPod{ns: "web", name: "api-2", kind: "Deployment", workload: "api", cpu: 500, mem: 1 << 30, labels: map[string]string{"app": "api"}})
		return CostPool{Name: "p", Nodes: 2, Priced: true, Hourly: 0.1}, []*planNode{a, b}
	}
	total := map[string]int{"web/Deployment/api": 2}

	// 2 → 1 node: still proposed, but low confidence, flagged, and it loses replica spread.
	pool, nodes := two()
	sc, ok := simulate("c1", pool, nodes, nodes[:1], total, nil, nil)
	if !ok {
		t.Fatal("2→1 should fit")
	}
	assess(&sc, pool, nodes, nodes[:1], &simCtx{})
	if sc.Confidence != "low" || !sc.RemovesHA || len(sc.Why) < 3 {
		t.Fatalf("2→1: %s HA=%v why=%+v", sc.Confidence, sc.RemovesHA, sc.Why)
	}
	lowWeight := sc.Weight

	// 3 → 2 with plenty of history and low peaks: high confidence, full weight.
	a, b, c := node("c1", "a", 0, -1), node("c1", "b", 0, -1), node("c1", "c", 0, -1)
	for i, n := range []*planNode{a, b, c} {
		put(n, &planPod{ns: "web", name: "api-" + string(rune('1'+i)), kind: "Deployment", workload: "api", cpu: 500, mem: 1 << 30})
	}
	pool3 := CostPool{Name: "p", Nodes: 3, Priced: true, Hourly: 0.1}
	all := []*planNode{a, b, c}
	sx := &simCtx{hours: 7 * 24, peaks: map[string]history.Peak{"web/api-1": {CPU95: 100, MemMax: 1 << 29, Buckets: 10}}}
	sc, _ = simulate("c1", pool3, all, all[:1], map[string]int{"web/Deployment/api": 3}, nil, nil)
	assess(&sc, pool3, all, all[:1], sx)
	if sc.Confidence != "high" || sc.RemovesHA || sc.Weight != sc.Savings || sc.Weight <= lowWeight {
		t.Fatalf("3→2: %s weight=%v why=%+v", sc.Confidence, sc.Weight, sc.Why)
	}

	// No history: medium. A PDB that allows no evictions: low.
	assess(&sc, pool3, all, all[:1], &simCtx{})
	if sc.Confidence != "medium" {
		t.Fatalf("no history: %s %+v", sc.Confidence, sc.Why)
	}
	a.pods[0].labels = map[string]string{"app": "api"}
	sel, _ := labels.Parse("app=api")
	assess(&sc, pool3, all, all[:1], &simCtx{hours: 200, peaks: sx.peaks, pdbs: []pdbInfo{{ns: "web", name: "api", sel: sel, allowed: 0}}})
	if sc.Confidence != "low" {
		t.Fatalf("blocking PDB: %s %+v", sc.Confidence, sc.Why)
	}

	// System pools need 3 nodes.
	sys := pool3
	sys.Mode = "system"
	assess(&sc, sys, all, all[:1], sx)
	if !sc.RemovesHA || sc.Confidence != "low" {
		t.Fatalf("system 3→2: %s HA=%v", sc.Confidence, sc.RemovesHA)
	}
}
