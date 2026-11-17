package monitor

import (
	"fmt"
	"slices"
	"strings"
	"sync"
)

// Node-removal scenarios for the clusters that carry every tag the user picked. Each scenario
// drains one or more worker nodes of a pool and re-places every replica they run onto other worker
// nodes — first within the same cluster, or, as an alternative, onto the other studied clusters
// (app workloads only). A replica only lands where its requests fit with 15% headroom, memory limits
// aren't overcommitted further, its node selector/affinity/taints allow it, and every disk it carries
// gets an attach slot under the VM's limit. Simulation only: nothing changes in any cluster.

const maxScenarios = 10

type ScenarioMove struct {
	Pod          string       `json:"pod"`
	Namespace    string       `json:"namespace"`
	Kind         string       `json:"kind"`
	Workload     string       `json:"workload"`
	Replicas     int          `json:"replicas"` // replicas the workload runs in its cluster
	CPU          int64        `json:"cpu"`
	Mem          int64        `json:"mem"`
	MemLim       int64        `json:"memLim"`
	Disks        int          `json:"disks"`
	Volumes      []PlanVolume `json:"volumes,omitempty"`
	FromNode     string       `json:"fromNode"`
	ToCluster    string       `json:"toCluster"`
	ToNode       string       `json:"toNode"`
	CrossCluster bool         `json:"crossCluster"`
}

type ScenarioNode struct {
	Cluster   string `json:"cluster"`
	Name      string `json:"name"`
	CPU       int64  `json:"cpu"` // requests after
	Mem       int64  `json:"mem"`
	MemLim    int64  `json:"memLim"`
	CPUBefore int64  `json:"cpuBefore"`
	MemBefore int64  `json:"memBefore"`
	AllocCPU  int64  `json:"allocCpu"`
	AllocMem  int64  `json:"allocMem"`
	Disks     int    `json:"disks"`
	DisksWas  int    `json:"disksBefore"`
	DiskLimit int    `json:"diskLimit"`
	Gains     int    `json:"gains"`
}

type Scenario struct {
	ID        string         `json:"id"`
	Cluster   string         `json:"cluster"`
	Pool      string         `json:"pool"`
	SKU       string         `json:"sku"`
	Region    string         `json:"region"`
	Strategy  string         `json:"strategy"` // in-cluster | spill
	Drain     []PlanDrain    `json:"drain"`
	NodesFrom int            `json:"nodesFrom"`
	NodesTo   int            `json:"nodesTo"`
	Savings   float64        `json:"savings"`
	Priced    bool           `json:"priced"`
	Moves     []ScenarioMove `json:"moves"`
	After     []ScenarioNode `json:"after"`
	// Comparison figures.
	Replicas      int      `json:"replicas"`
	Workloads     int      `json:"workloads"`
	CrossCluster  int      `json:"crossCluster"`  // replicas that leave the cluster
	DisksMoved    int      `json:"disksMoved"`    // disks that re-attach elsewhere (same cluster)
	DataMigration int      `json:"dataMigration"` // volumes whose data must be copied (cross-cluster)
	Outages       int      `json:"outages"`       // single-replica workloads that restart
	TargetsUsed   []string `json:"targetsUsed"`   // clusters receiving replicas
	Notes         []string `json:"notes"`
	Commands      []string `json:"commands"`
}

// NamespaceInfo is a namespace found in the studied clusters, for the namespace picker.
type NamespaceInfo struct {
	Name     string `json:"name"`
	System   bool   `json:"system"`   // platform or cluster add-on namespace
	Pods     int    `json:"pods"`     // non-DaemonSet pods across studied clusters
	Clusters int    `json:"clusters"` // studied clusters that have it
}

type ScenarioReport struct {
	Picked     []string            `json:"picked"`
	Namespaces []NamespaceInfo     `json:"namespaces"` // in the studied clusters
	Studied    []string            `json:"studiedNamespaces"`
	Tags       map[string][]string `json:"tags"`     // every tag → its connected clusters
	Clusters   []string            `json:"clusters"` // clusters carrying all picked tags
	Scenarios  []Scenario          `json:"scenarios"`
	Considered int                 `json:"considered"` // feasible scenarios before the top-10 cut
}

// CostScenarios studies the clusters carrying every picked tag. Only pods in `namespaces` are
// simulated; with none given, every app namespace (system and add-on namespaces left out).
func (m *Manager) CostScenarios(picked, namespaces []string) ScenarioReport {
	s := m.settings.Get()
	rep := ScenarioReport{Picked: picked, Tags: map[string][]string{}, Clusters: []string{}, Scenarios: []Scenario{},
		Namespaces: []NamespaceInfo{}, Studied: []string{}}
	type member struct {
		c     *cluster
		cost  CostCluster
		nodes []*planNode
		pods  []*planPod
	}
	var members []*member
	for _, c := range m.connected() {
		tags := s.ClusterTags[c.name]
		for _, t := range tags {
			rep.Tags[t] = append(rep.Tags[t], c.name)
		}
		if len(picked) == 0 || !c.synced("pods") || !c.synced("nodes") {
			continue
		}
		if !slices.ContainsFunc(picked, func(t string) bool { return !slices.Contains(tags, t) }) {
			members = append(members, &member{c: c})
		}
	}
	for t := range rep.Tags {
		slices.Sort(rep.Tags[t])
	}
	var wg sync.WaitGroup
	for _, mb := range members {
		wg.Go(func() {
			mb.cost = mb.c.costCluster(nil, 0)
			mb.nodes, mb.pods = mb.c.planState()
		})
	}
	wg.Wait()
	slices.SortFunc(members, func(a, b *member) int { return strings.Compare(a.c.name, b.c.name) })
	nsInfo := map[string]*NamespaceInfo{}
	for _, mb := range members {
		rep.Clusters = append(rep.Clusters, mb.c.name)
		seen := map[string]bool{}
		for _, p := range mb.pods {
			if p.ds {
				continue
			}
			ni := nsInfo[p.ns]
			if ni == nil {
				ni = &NamespaceInfo{Name: p.ns, System: systemNamespace(p.ns)}
				nsInfo[p.ns] = ni
			}
			ni.Pods++
			if !seen[p.ns] {
				seen[p.ns] = true
				ni.Clusters++
			}
		}
	}
	study := map[string]bool{}
	for _, ni := range nsInfo {
		rep.Namespaces = append(rep.Namespaces, *ni)
		if len(namespaces) == 0 && !ni.System || slices.Contains(namespaces, ni.Name) {
			study[ni.Name] = true
			rep.Studied = append(rep.Studied, ni.Name)
		}
	}
	slices.SortFunc(rep.Namespaces, func(a, b NamespaceInfo) int { return strings.Compare(a.Name, b.Name) })
	slices.Sort(rep.Studied)
	var all []Scenario
	for i, mb := range members {
		var others []*planNode
		for j, o := range members {
			if j != i {
				others = append(others, o.nodes...)
			}
		}
		for _, pool := range mb.cost.Pools {
			all = append(all, poolScenarios(mb.c.name, pool, mb.nodes, mb.pods, others, study)...)
		}
	}
	rep.Considered = len(all)
	slices.SortFunc(all, compareScenarios)
	// Variety: at most 2 scenarios per pool and size, so ten picks don't all say the same thing.
	seen := map[string]int{}
	for _, sc := range all {
		key := fmt.Sprintf("%s/%s/%d/%s", sc.Cluster, sc.Pool, len(sc.Drain), sc.Strategy)
		if seen[key] >= 2 || len(rep.Scenarios) >= maxScenarios {
			continue
		}
		seen[key]++
		rep.Scenarios = append(rep.Scenarios, sc)
	}
	return rep
}

// compareScenarios: bigger saving first; then staying in-cluster, fewer replicas moving, fewer
// disks, fewer outages.
func compareScenarios(a, b Scenario) int {
	if d := cmpF(b.Savings, a.Savings); d != 0 {
		return d
	}
	if d := len(b.Drain) - len(a.Drain); d != 0 {
		return d
	}
	if a.Strategy != b.Strategy {
		if a.Strategy == "in-cluster" {
			return -1
		}
		return 1
	}
	for _, d := range []int{a.Replicas - b.Replicas, a.DisksMoved + a.DataMigration - b.DisksMoved - b.DataMigration, a.Outages - b.Outages} {
		if d != 0 {
			return d
		}
	}
	return strings.Compare(a.ID, b.ID)
}

// poolScenarios tries draining 1..n-1 nodes of a pool: each single node on its own, and the least
// loaded k nodes for larger k; in-cluster first, then spilling app replicas to the other clusters.
func poolScenarios(cluster string, pool CostPool, nodes []*planNode, pods []*planPod, others []*planNode, study map[string]bool) []Scenario {
	var inPool []*planNode
	for _, n := range nodes {
		if n.pool == pool.Name {
			inPool = append(inPool, n)
		}
	}
	if len(inPool) < 2 {
		return nil // keep at least one node per pool
	}
	load := func(n *planNode) float64 {
		return float64(n.usedCPU)/float64(max(n.allocCPU, 1)) + float64(n.usedMem)/float64(max(n.allocMem, 1)) + float64(n.disks)*0.02
	}
	sorted := slices.Clone(inPool)
	slices.SortFunc(sorted, func(a, b *planNode) int { return cmpF(load(a), load(b)) })
	var sets [][]*planNode
	for _, n := range sorted { // every single node
		sets = append(sets, []*planNode{n})
	}
	for k := 2; k < len(sorted); k++ { // the k least loaded
		sets = append(sets, sorted[:k])
	}
	total := map[string]int{}
	for _, p := range pods {
		total[p.ns+"/"+p.kind+"/"+p.workload]++
	}
	var out []Scenario
	for _, drain := range sets {
		if slices.ContainsFunc(drain, func(n *planNode) bool {
			return slices.ContainsFunc(n.pods, func(p *planPod) bool { return p.pinned && !p.ds && study[p.ns] })
		}) {
			continue // a studied pod tied to this node (hostPath, hostname) can't move
		}
		sc, ok := simulate(cluster, pool, nodes, drain, total, nil, study)
		if !ok && len(others) > 0 {
			sc, ok = simulate(cluster, pool, nodes, drain, total, others, study)
		}
		if ok && sc.Replicas == 0 {
			continue // nothing studied runs there: not a useful scenario for this selection
		}
		if ok {
			out = append(out, sc)
		}
	}
	return out
}

// simulate drains `drain` and places its pods: on the cluster's other nodes, and, when `others` is
// given, app replicas that don't fit there go to the other studied clusters' nodes.
func simulate(cluster string, pool CostPool, nodes, drain []*planNode, total map[string]int, others []*planNode, study map[string]bool) (Scenario, bool) {
	gone := map[string]bool{}
	for _, n := range drain {
		gone[n.name] = true
	}
	var local []*planNode
	for _, n := range nodes {
		if !gone[n.name] {
			local = append(local, n)
		}
	}
	local = cloneNodes(local)
	remote := cloneNodes(others)
	before := map[*planNode][3]int64{}
	for _, n := range append(slices.Clone(local), remote...) {
		before[n] = [3]int64{n.usedCPU, n.usedMem, int64(n.disks)}
	}
	var moving, background []*planPod
	for _, n := range drain {
		for _, p := range n.pods {
			switch {
			case p.ds:
			case study == nil || study[p.ns]:
				moving = append(moving, p)
			default:
				background = append(background, p)
			}
		}
	}
	other := len(background)
	// Pods outside the studied namespaces aren't listed, but Kubernetes reschedules them too: they
	// must still fit on the cluster's remaining nodes, or the scenario isn't real.
	for _, p := range background {
		if p.pinned {
			return Scenario{}, false
		}
		dst := bestFit(p, pool.Name, local)
		if dst == nil {
			return Scenario{}, false
		}
		dst.take(p)
	}
	// Disk-carrying and bigger pods first: disk slots and big gaps are the scarce part.
	slices.SortFunc(moving, func(a, b *planPod) int {
		if d := len(b.disks) - len(a.disks); d != 0 {
			return d
		}
		return int((b.cpu + b.mem>>20) - (a.cpu + a.mem>>20))
	})
	sc := Scenario{Cluster: cluster, Pool: pool.Name, SKU: pool.SKU, Region: pool.Region, Strategy: "in-cluster",
		NodesFrom: pool.Nodes, NodesTo: pool.Nodes - len(drain), Drain: []PlanDrain{}, Moves: []ScenarioMove{}, After: []ScenarioNode{},
		TargetsUsed: []string{}, Notes: []string{}}
	gains := map[*planNode]int{}
	workloads := map[string]bool{}
	outageSeen := map[string]bool{}
	for _, p := range moving {
		dst := bestFit(p, pool.Name, local)
		cross := false
		if dst == nil && others != nil && !p.affinity && !systemNamespace(p.ns) && remoteOK(p) {
			dst, cross = bestFit(p, pool.Name, remote), true
		}
		if dst == nil {
			return Scenario{}, false
		}
		dst.take(p)
		gains[dst]++
		key := p.ns + "/" + p.kind + "/" + p.workload
		workloads[key] = true
		mv := ScenarioMove{Pod: p.name, Namespace: p.ns, Kind: p.kind, Workload: p.workload, Replicas: total[key], CPU: p.cpu, Mem: p.mem,
			MemLim: p.memLim, Disks: len(p.disks), Volumes: p.pvcs, FromNode: p.node, ToCluster: dst.cluster, ToNode: dst.name, CrossCluster: cross}
		sc.Moves = append(sc.Moves, mv)
		if cross {
			sc.Strategy = "spill"
			sc.CrossCluster++
			sc.DataMigration += len(p.pvcs)
			if !slices.Contains(sc.TargetsUsed, dst.cluster) {
				sc.TargetsUsed = append(sc.TargetsUsed, dst.cluster)
			}
		} else {
			sc.DisksMoved += len(p.disks)
		}
		if total[key] <= 1 && !outageSeen[key] {
			outageSeen[key] = true
			sc.Outages++
		}
	}
	sc.Replicas, sc.Workloads = len(moving), len(workloads)
	for _, n := range drain {
		d := PlanDrain{Name: n.name, Disks: n.disks}
		for _, p := range n.pods {
			switch {
			case p.ds:
			case study == nil || study[p.ns]:
				d.Pods++
			default:
				d.Other++
			}
		}
		sc.Drain = append(sc.Drain, d)
	}
	if other > 0 {
		sc.Notes = append(sc.Notes, fmt.Sprintf("%d other pod(s) on the drained node(s) are in system/add-on or unselected namespaces: not listed, but checked to fit on the remaining nodes, where Kubernetes reschedules them.", other))
	}
	for n, g := range gains {
		b := before[n]
		sc.After = append(sc.After, ScenarioNode{Cluster: n.cluster, Name: n.name, CPU: n.usedCPU, Mem: n.usedMem, MemLim: n.limMem,
			CPUBefore: b[0], MemBefore: b[1], AllocCPU: n.allocCPU, AllocMem: n.allocMem, Disks: n.disks, DisksWas: int(b[2]), DiskLimit: n.diskLimit, Gains: g})
	}
	slices.SortFunc(sc.After, func(a, b ScenarioNode) int { return strings.Compare(a.Cluster+a.Name, b.Cluster+b.Name) })
	slices.SortFunc(sc.Moves, func(a, b ScenarioMove) int {
		return strings.Compare(a.Namespace+a.Kind+a.Workload+a.Pod, b.Namespace+b.Kind+b.Workload+b.Pod)
	})
	if pool.Priced {
		sc.Savings, sc.Priced = pool.Hourly*hoursPerMonth*float64(len(drain)), true
	}
	names := make([]string, len(drain))
	for i, n := range drain {
		names[i] = n.name
	}
	sc.ID = cluster + "/" + pool.Name + "/" + sc.Strategy + "/" + strings.Join(names, "+")
	notes, cmds := scenarioNotes(sc, pool)
	sc.Notes, sc.Commands = append(notes, sc.Notes...), cmds
	return sc, true
}

// remoteOK: a pod may go to another cluster only if nothing ties it to this cluster's nodes.
func remoteOK(p *planPod) bool {
	for k := range p.selector {
		if k != "kubernetes.io/os" && k != "kubernetes.io/arch" {
			return false
		}
	}
	return !p.pinned
}

func scenarioNotes(sc Scenario, pool CostPool) ([]string, []string) {
	notes := []string{"Requests fit with 15% headroom on every receiving node, memory limits stay within the overcommit each cluster already runs with, and every disk has an attach slot under the VM's limit."}
	for _, a := range sc.After {
		if a.AllocMem > 0 && a.MemLim > a.AllocMem {
			notes = append(notes, fmt.Sprintf("Memory limits on the receiving nodes go above their allocatable memory (overcommitted, as the cluster already is): if every pod used its full limit at once, some would be OOM-killed."))
			break
		}
	}
	if sc.DisksMoved > 0 {
		notes = append(notes, fmt.Sprintf("%d disk(s) detach from the drained node(s) and re-attach on the new ones; those pods restart. Zonal disks can only follow to nodes in the same zone.", sc.DisksMoved))
	}
	if sc.CrossCluster > 0 {
		notes = append(notes, fmt.Sprintf("%d replica(s) move to %s: scale those workloads up there first (each replica serves load today).", sc.CrossCluster, strings.Join(sc.TargetsUsed, ", ")))
		if sc.DataMigration > 0 {
			notes = append(notes, fmt.Sprintf("%d volume(s) can't follow across clusters: their data must be copied to new volumes in the target first.", sc.DataMigration))
		}
	}
	if sc.Outages > 0 {
		notes = append(notes, fmt.Sprintf("%d single-replica workload(s) restart while they move: a short outage each.", sc.Outages))
	}
	var cmds []string
	for _, d := range sc.Drain {
		cmds = append(cmds, kubectl(sc.Cluster, "", "cordon "+d.Name))
	}
	if sc.CrossCluster > 0 {
		cmds = append(cmds, "# scale up / deploy the moving workloads in "+strings.Join(sc.TargetsUsed, ", ")+" and migrate their volume data")
	}
	for _, d := range sc.Drain {
		cmds = append(cmds, kubectl(sc.Cluster, "", "drain "+d.Name+" --ignore-daemonsets --delete-emptydir-data --timeout=10m"))
	}
	switch pool.Provider {
	case "azure":
		cmds = append(cmds, fmt.Sprintf("az aks nodepool scale --cluster-name <aks-name> --resource-group <rg> --name %s --node-count %d", pool.Name, sc.NodesTo))
	case "aws":
		cmds = append(cmds, fmt.Sprintf("eksctl scale nodegroup --cluster <eks-name> --name %s --nodes %d", pool.Name, sc.NodesTo))
	default:
		cmds = append(cmds, fmt.Sprintf("# then set node pool %q to %d nodes in your provider (or lower its autoscaler minimum)", pool.Name, sc.NodesTo))
	}
	return notes, cmds
}
