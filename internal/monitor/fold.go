package monitor

import (
	"fmt"
	"slices"
	"strings"
	"sync"
)

// Fold alternatives for one tag: for each cluster you might keep, retire as many of the other
// tagged clusters into it as fit — each one simulated on top of the previous, so their replicas and
// disks share the kept cluster's real nodes. Every replica moves (shared workloads scale up in the
// target); system pieces and cluster add-ons stay, since the kept cluster runs its own.

type foldSource struct {
	c    *cluster
	cost CostCluster
}

// FoldTags lists tags shared by at least two connected clusters, with their clusters.
func (m *Manager) FoldTags() map[string][]string {
	s := m.settings.Get()
	out := map[string][]string{}
	for _, c := range m.connected() {
		for _, t := range s.ClusterTags[c.name] {
			out[t] = append(out[t], c.name)
		}
	}
	for t, cs := range out {
		if len(cs) < 2 {
			delete(out, t)
		} else {
			slices.Sort(cs)
		}
	}
	return out
}

// FoldPlans returns one alternative per cluster that could stay, best saving first.
func (m *Manager) FoldPlans(tag string) []CostPlan {
	s := m.settings.Get()
	var members []foldSource
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range m.connected() {
		if !slices.Contains(s.ClusterTags[c.name], tag) || !c.synced("pods") || !c.synced("nodes") {
			continue
		}
		wg.Go(func() {
			cc, _ := c.costCluster(nil, 0)
			mu.Lock()
			members = append(members, foldSource{c, cc})
			mu.Unlock()
		})
	}
	wg.Wait()
	slices.SortFunc(members, func(a, b foldSource) int { return strings.Compare(a.cost.Cluster, b.cost.Cluster) })
	out := []CostPlan{}
	for i, keep := range members {
		var others []foldSource
		for j, o := range members {
			if j != i {
				others = append(others, o)
			}
		}
		// Retire the most expensive clusters first: they save the most if they fit.
		slices.SortFunc(others, func(a, b foldSource) int { return cmpF(b.cost.Monthly, a.cost.Monthly) })
		if p, ok := foldInto(tag, keep, others); ok {
			out = append(out, *p)
		}
	}
	slices.SortFunc(out, func(a, b CostPlan) int {
		if d := cmpF(b.Savings, a.Savings); d != 0 {
			return d
		}
		return len(b.Retire) - len(a.Retire)
	})
	return out
}

func foldInto(tag string, keep foldSource, others []foldSource) (*CostPlan, bool) {
	dstNodes, dstPods := keep.c.planState()
	inTarget := map[string]int{}
	for _, p := range dstPods {
		inTarget[p.ns+"/"+p.kind+"/"+p.workload]++
	}
	state := cloneNodes(dstNodes)
	plan := &CostPlan{ID: "fold/" + tag + "/" + keep.c.name, Type: "move-cluster", Target: keep.c.name, Tag: tag,
		Drain: []PlanDrain{}, After: []PlanNodeAfter{}, Merged: []string{}, Skipped: []string{}, Notes: []string{}, Retire: []string{}, Priced: true}
	gains := map[string]int{}
	added := map[string]int{} // replicas of a workload already folded in from earlier sources
	seenSkip, seenMerge := map[string]bool{}, map[string]bool{}
	var unfit []string
	vols, pinned := 0, 0
	keepRegion := regionOf(keep.cost)
	for _, src := range others {
		_, srcPods := src.c.planState()
		var moving []*planPod
		var skipped []string
		for _, p := range srcPods {
			key := p.ns + "/" + p.kind + "/" + p.workload
			if p.ds || systemNamespace(p.ns) {
				skipped = append(skipped, key)
				continue
			}
			moving = append(moving, p)
		}
		if len(moving) == 0 {
			continue
		}
		trial := cloneNodes(state)
		where, ok := placeAll(moving, trial)
		if !ok {
			unfit = append(unfit, src.c.name)
			continue
		}
		state = trial
		for _, n := range where {
			gains[n.name]++
		}
		moves := groupMoves(moving, where, srcPods)
		for i := range moves {
			mv := &moves[i]
			key := mv.Namespace + "/" + mv.Kind + "/" + mv.Name
			mv.Source = src.c.name
			mv.TargetReplicas = inTarget[key] + added[key]
			added[key] += mv.Replicas
			if mv.TargetReplicas > 0 && !seenMerge[key] {
				seenMerge[key] = true
				plan.Merged = append(plan.Merged, key)
			}
			vols += len(mv.Volumes)
		}
		for _, p := range moving {
			if p.pinned {
				pinned++
			}
		}
		for _, k := range skipped {
			if !seenSkip[k] {
				seenSkip[k] = true
				plan.Skipped = append(plan.Skipped, k)
			}
		}
		plan.Moves = append(plan.Moves, moves...)
		plan.Retire = append(plan.Retire, src.c.name)
		plan.NodesFrom += src.cost.Nodes
		plan.Savings += src.cost.Monthly
		plan.Priced = plan.Priced && src.cost.Priced
		if r := regionOf(src.cost); r != "" && keepRegion != "" && r != keepRegion {
			plan.Notes = append(plan.Notes, fmt.Sprintf("⚠ %s runs in %s and %s in %s: a second region is often there on purpose (redundancy, latency, data residency). Merging gives that up.", src.c.name, r, keep.c.name, keepRegion))
		}
	}
	if len(plan.Retire) == 0 {
		return nil, false
	}
	plan.Cluster = strings.Join(plan.Retire, ", ")
	for _, n := range state {
		if gains[n.name] == 0 {
			continue
		}
		plan.After = append(plan.After, PlanNodeAfter{Name: n.name, CPU: n.usedCPU, Mem: n.usedMem, AllocCPU: n.allocCPU, AllocMem: n.allocMem,
			Disks: n.disks, DiskLimit: n.diskLimit, Gains: gains[n.name]})
	}
	slices.SortFunc(plan.After, func(a, b PlanNodeAfter) int { return strings.Compare(a.Name, b.Name) })
	slices.Sort(plan.Merged)
	slices.Sort(plan.Skipped)
	plan.Notes = append(plan.Notes, "Replicas are placed on "+keep.c.name+"'s current nodes with 15% headroom, within each VM's disk-attach limit; each retired cluster is added on top of the previous one.")
	if len(plan.Merged) > 0 {
		plan.Notes = append(plan.Notes, fmt.Sprintf("%d workload(s) also run in %s: they keep every replica, so scale %s's copy up as shown per workload, and check the copies are configured the same.", len(plan.Merged), keep.c.name, keep.c.name))
	}
	if vols > 0 {
		plan.Notes = append(plan.Notes, fmt.Sprintf("%d volume(s) hold data: copy it to new volumes in %s before switching over.", vols, keep.c.name))
	}
	if pinned > 0 {
		plan.Notes = append(plan.Notes, fmt.Sprintf("%d pod(s) use hostPath or are pinned to a node: check they can run on %s's nodes.", pinned, keep.c.name))
	}
	if len(unfit) > 0 {
		plan.Notes = append(plan.Notes, fmt.Sprintf("Doesn't fit as well: %s (not enough room left in %s after the others).", strings.Join(unfit, ", "), keep.c.name))
	}
	plan.Commands = []string{
		"# 1. deploy the moving workloads into " + keep.c.name + " (from GitOps/Helm, or exported manifests), scaling shared ones up as listed",
		"# 2. migrate volume data, then move traffic (DNS / ingress)",
		"# 3. once " + keep.c.name + " serves everything, retire " + strings.Join(plan.Retire, ", "),
	}
	return plan, true
}
