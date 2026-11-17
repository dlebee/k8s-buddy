package monitor

import (
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/labels"
)

// How sure a scenario is: every scenario that fits is proposed, but losing high availability, thin
// usage history, peaks near capacity, single replicas, blocking PodDisruptionBudgets and lost
// replica or zone spread lower its confidence, and so its rank.

var confRank = map[string]int{"high": 3, "medium": 2, "low": 1}
var confFactor = map[string]float64{"high": 1, "medium": 0.6, "low": 0.25}

// minHA is the smallest pool that still survives losing a node: 2, or 3 for an AKS system pool.
func minHA(pool CostPool) int {
	if pool.Mode == "system" {
		return 3
	}
	return 2
}

func assess(sc *Scenario, pool CostPool, nodes, drain []*planNode, sx *simCtx) {
	if sx == nil {
		sx = &simCtx{}
	}
	var why []ConfReason
	var short string // label for the next reason
	add := func(level, f string, a ...any) {
		why = append(why, ConfReason{Level: level, Text: fmt.Sprintf(f, a...), Short: short})
		short = ""
	}
	low := func(f string, a ...any) { add("low", f, a...) }
	medium := func(f string, a ...any) { add("medium", f, a...) }
	label := func(s string, a ...any) { short = fmt.Sprintf(s, a...) }
	sc.Basis = []string{}

	// High availability of the pool itself.
	if m := minHA(pool); sc.NodesTo < m {
		sc.RemovesHA = sc.NodesFrom >= m
		kind := "pool"
		if pool.Mode == "system" {
			kind = "system pool (runs CoreDNS and other cluster add-ons)"
		}
		label("removes high availability")
		if sc.NodesTo <= 1 {
			low("Removes high availability: the %s drops to %d node, so one node failure, reboot or upgrade takes its workloads down.", kind, sc.NodesTo)
		} else {
			label("removes high availability")
			low("Removes high availability: the %s drops to %d nodes, below the %d recommended to survive losing one.", kind, sc.NodesTo, m)
		}
	} else {
		sc.Basis = append(sc.Basis, fmt.Sprintf("Pool keeps %d nodes", sc.NodesTo))
	}

	gone := map[string]bool{}
	for _, n := range drain {
		gone[n.name] = true
	}

	// Zone spread of the pool.
	zones := func(skip bool) int {
		z := map[string]bool{}
		for _, n := range nodes {
			if n.pool != pool.Name || (skip && gone[n.name]) {
				continue
			}
			if v := n.labels["topology.kubernetes.io/zone"]; v != "" && v != "0" {
				z[v] = true
			}
		}
		return len(z)
	}
	if zb, za := zones(false), zones(true); zb > 1 && za < zb {
		if za <= 1 {
			label("single zone left")
			low("The pool's remaining nodes are all in one zone (it spans %d today): a zone outage takes it down.", zb)
		} else {
			label("%d → %d zones", zb, za)
			medium("The pool spans %d zones instead of %d.", za, zb)
		}
	}

	// Replica spread: workloads whose replicas all end up on one node.
	type wl struct{ before, after map[string]bool }
	spread := map[string]*wl{}
	get := func(k string) *wl {
		if spread[k] == nil {
			spread[k] = &wl{map[string]bool{}, map[string]bool{}}
		}
		return spread[k]
	}
	moved := map[string]bool{}
	for _, mv := range sc.Moves {
		k := mv.Namespace + "/" + mv.Kind + "/" + mv.Workload
		moved[k] = true
		get(k).after[mv.ToCluster+"/"+mv.ToNode] = true
	}
	for _, n := range nodes {
		for _, p := range n.pods {
			k := p.ns + "/" + p.kind + "/" + p.workload
			if p.ds || !moved[k] {
				continue
			}
			w := get(k)
			w.before[n.name] = true
			if !gone[n.name] {
				w.after[n.cluster+"/"+n.name] = true
			}
		}
	}
	var packed []string
	for k, w := range spread {
		if len(w.before) > 1 && len(w.after) == 1 {
			packed = append(packed, k)
		}
	}
	if len(packed) > 0 {
		slices.Sort(packed)
		label("%d workload(s) lose replica spread", len(packed))
		medium("%d workload(s) end up with every replica on one node, so one node failure takes them down (e.g. %s).", len(packed), workloadLabel(packed[0]))
	}

	if sc.Outages > 0 {
		medium("%d single-replica workload(s) go down briefly while they move.", sc.Outages)
	}
	if sc.CrossCluster > 0 {
		medium("%d replica(s) must be deployed in another cluster first.", sc.CrossCluster)
	}
	if sc.DataMigration > 0 {
		low("%d volume(s) need their data copied to another cluster.", sc.DataMigration)
	}

	// PodDisruptionBudgets that allow no eviction now would stall the drain.
	blocked := map[string]bool{}
	for _, n := range drain {
		for _, p := range n.pods {
			if p.ds {
				continue
			}
			for _, b := range sx.pdbs {
				if b.ns == p.ns && b.allowed <= 0 && b.sel.Matches(labels.Set(p.labels)) {
					blocked[b.ns+"/"+b.name] = true
				}
			}
		}
	}
	if len(blocked) > 0 {
		names := make([]string, 0, len(blocked))
		for k := range blocked {
			names = append(names, k)
		}
		slices.Sort(names)
		label("PDB blocks the drain")
		low("PodDisruptionBudget %s allows no evictions right now: the drain would stall until it does.", strings.Join(names, ", "))
	}

	// Usage history: peaks on the receiving nodes of this cluster.
	switch {
	case len(sx.peaks) == 0 || sx.hours < 24:
		label("no usage history")
		if len(sx.peaks) > 0 {
			label("%.0fh of history", sx.hours)
		}
		medium("Less than a day of usage history: judged on requests only, not on real peaks.")
	case sx.hours < 72:
		label("%.0fh of history", sx.hours)
		medium("Only %.0f hours of usage history: weekly peaks may be missing.", sx.hours)
		peakCheck(sc, nodes, sx, medium, low, label)
	default:
		sc.Basis = append(sc.Basis, fmt.Sprintf("%.0f days of usage history", sx.hours/24))
		peakCheck(sc, nodes, sx, medium, low, label)
	}

	sc.Confidence = "high"
	for _, r := range why {
		if confRank[r.Level] < confRank[sc.Confidence] {
			sc.Confidence = r.Level
		}
	}
	slices.SortStableFunc(why, func(a, b ConfReason) int { return confRank[a.Level] - confRank[b.Level] })
	sc.Why = why
	if sc.Why == nil {
		sc.Why = []ConfReason{}
	}
	if sc.Priced {
		sc.Weight = sc.Savings * confFactor[sc.Confidence]
	} else {
		sc.Weight = float64(len(sc.Drain)) * 1e-3 * confFactor[sc.Confidence] // below any priced scenario
	}
}

// peakCheck adds up recorded peaks (per pod: highest memory, 95th-percentile CPU; requests where a
// pod has no history) on each receiving node of the scenario's cluster.
func peakCheck(sc *Scenario, nodes []*planNode, sx *simCtx, medium, low func(string, ...any), label func(string, ...any)) {
	byName := map[string]*planNode{}
	for _, n := range nodes {
		byName[n.name] = n
	}
	incoming := map[string][][2]float64{}
	for _, mv := range sc.Moves {
		if mv.ToCluster != sc.Cluster {
			continue
		}
		cpu, mem := float64(mv.CPU), float64(mv.Mem)
		if pk, ok := sx.peaks[mv.Namespace+"/"+mv.Pod]; ok && pk.Buckets > 0 {
			cpu, mem = pk.CPU95, pk.MemMax
		}
		incoming[mv.ToNode] = append(incoming[mv.ToNode], [2]float64{cpu, mem})
	}
	worstMem, worstCPU := 0.0, 0.0
	var memNode, cpuNode string
	for name, add := range incoming {
		n := byName[name]
		if n == nil || n.allocCPU <= 0 || n.allocMem <= 0 {
			continue
		}
		var cpu, mem float64
		for _, p := range n.pods {
			c, m := float64(p.cpu), float64(p.mem)
			if pk, ok := sx.peaks[p.ns+"/"+p.name]; ok && pk.Buckets > 0 {
				c, m = pk.CPU95, pk.MemMax
			}
			cpu, mem = cpu+c, mem+m
		}
		for _, a := range add {
			cpu, mem = cpu+a[0], mem+a[1]
		}
		if r := mem / float64(n.allocMem); r > worstMem {
			worstMem, memNode = r, name
		}
		if r := cpu / float64(n.allocCPU); r > worstCPU {
			worstCPU, cpuNode = r, name
		}
	}
	switch {
	case worstMem >= 0.95:
		label("peak memory %.0f%%", worstMem*100)
		low("At recorded peaks, memory on %s would reach %.0f%% of allocatable: pods risk being OOM-killed or evicted.", memNode, worstMem*100)
	case worstMem >= 0.85:
		label("peak memory %.0f%%", worstMem*100)
		medium("At recorded peaks, memory on %s would reach %.0f%% of allocatable.", memNode, worstMem*100)
	}
	if worstCPU >= 1 {
		label("peak CPU %.0f%%", worstCPU*100)
		medium("At recorded peaks, CPU on %s would reach %.0f%% of allocatable: pods would be throttled.", cpuNode, worstCPU*100)
	}
	if worstMem > 0 && worstMem < 0.85 && worstCPU < 1 {
		sc.Basis = append(sc.Basis, fmt.Sprintf("peaks stay under %.0f%% memory on receiving nodes", worstMem*100+0.5))
	}
}

// workloadLabel turns "ns/Kind/name" into "ns/name".
func workloadLabel(k string) string {
	parts := strings.SplitN(k, "/", 3)
	if len(parts) == 3 {
		return parts[0] + "/" + parts[2]
	}
	return k
}
