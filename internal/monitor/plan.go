package monitor

import (
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// Concrete cost plans, simulated against the pods that run right now: which nodes to drain, which
// workload replicas move where, and which disks re-attach where, within every VM's disk-attach
// limit. A plan is only offered when every pod and disk finds room.

// planPod is one pod as the simulator sees it.
type planPod struct {
	ns, name, kind, workload string
	node                     string
	cpu, mem                 int64
	disks                    []string // attached disks (PV names) this pod brings along
	pvcs                     []PlanVolume
	ds, pinned               bool
	pinReason                string
}

type planNode struct {
	name, pool         string
	allocCPU, allocMem int64
	usedCPU, usedMem   int64 // requests of the pods on it (DaemonSets included)
	disks, diskLimit   int   // attached now; -1 limit unknown
	pods               []*planPod
	sku                NodeSKU
}

type PlanVolume struct {
	Claim        string `json:"claim"`
	Size         string `json:"size"`
	StorageClass string `json:"storageClass"`
	Disk         bool   `json:"disk"` // attached to the node as a disk (counts against the VM limit)
}

type PlanMove struct {
	Namespace     string       `json:"namespace"`
	Kind          string       `json:"kind"`
	Name          string       `json:"name"`
	Replicas      int          `json:"replicas"`      // replicas that move
	TotalReplicas int          `json:"totalReplicas"` // replicas the workload has in the cluster
	CPU           int64        `json:"cpu"`           // per replica
	Mem           int64        `json:"mem"`
	Disks         int          `json:"disks"` // disks moving with these replicas
	From          []string     `json:"from"`
	To            []string     `json:"to"` // target node per moving replica
	Volumes       []PlanVolume `json:"volumes,omitempty"`
	// TargetReplicas: replicas the same workload already runs in the target cluster (move plans).
	// The moving replicas are added on top: the target's copy scales from TargetReplicas to
	// TargetReplicas+Replicas, because those replicas carry the source cluster's load.
	TargetReplicas int    `json:"targetReplicas,omitempty"`
	Source         string `json:"source,omitempty"` // fold plans: the cluster these replicas come from
}

type PlanNodeAfter struct {
	Name      string `json:"name"`
	CPU       int64  `json:"cpu"` // requests after the plan
	Mem       int64  `json:"mem"`
	AllocCPU  int64  `json:"allocCpu"`
	AllocMem  int64  `json:"allocMem"`
	Disks     int    `json:"disks"`
	DiskLimit int    `json:"diskLimit"`
	Gains     int    `json:"gains"` // pods it receives
}

type PlanDrain struct {
	Name  string `json:"name"`
	Pods  int    `json:"pods"` // non-DaemonSet pods that must move
	Disks int    `json:"disks"`
}

type CostPlan struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"` // shrink-pool | move-cluster
	Cluster   string          `json:"cluster"`
	Pool      string          `json:"pool,omitempty"`
	Target    string          `json:"target,omitempty"` // move-cluster: destination cluster
	Tag       string          `json:"tag,omitempty"`
	SKU       string          `json:"sku,omitempty"`
	NodesFrom int             `json:"nodesFrom"`
	NodesTo   int             `json:"nodesTo"`
	Drain     []PlanDrain     `json:"drain"`
	Moves     []PlanMove      `json:"moves"`
	After     []PlanNodeAfter `json:"after"`
	Merged    []string        `json:"merged,omitempty"`  // workloads that also run in the target: scaled up there, not dropped
	Retire    []string        `json:"retire,omitempty"`  // fold plans: clusters retired into Target
	Skipped   []string        `json:"skipped,omitempty"` // system workloads not moved
	Savings   float64         `json:"savings"`
	Priced    bool            `json:"priced"`
	Commands  []string        `json:"commands"`
	Notes     []string        `json:"notes"`
}

// planState snapshots a cluster for simulation.
func (c *cluster) planState() ([]*planNode, []*planPod) {
	nodes, _ := c.nodes.List(labels.Everything())
	pods, _ := c.pods.List(labels.Everything())
	pvcSize := map[string]PlanVolume{} // ns/claim → details
	pvOf := map[string]string{}        // ns/claim → PV name
	if c.synced("pvcs") {
		claims, _ := c.pvcs.List(labels.Everything())
		for _, p := range claims {
			sz := p.Spec.Resources.Requests[corev1.ResourceStorage]
			sc := ""
			if p.Spec.StorageClassName != nil {
				sc = *p.Spec.StorageClassName
			}
			pvcSize[p.Namespace+"/"+p.Name] = PlanVolume{Claim: p.Name, Size: sz.String(), StorageClass: sc}
			pvOf[p.Namespace+"/"+p.Name] = p.Spec.VolumeName
		}
	}
	byName := map[string]*planNode{}
	attached := map[string]bool{} // PV names attached as disks anywhere
	var out []*planNode
	for _, n := range nodes {
		cpu, mem := n.Status.Allocatable[corev1.ResourceCPU], n.Status.Allocatable[corev1.ResourceMemory]
		d := nodeDisks(n, c.csiNode(n.Name))
		for _, dr := range d.Drivers {
			for _, v := range dr.Volumes {
				attached[v] = true
			}
		}
		pn := &planNode{name: n.Name, pool: poolOf(n), allocCPU: cpu.MilliValue(), allocMem: mem.Value(), disks: d.Attached, diskLimit: d.Limit, sku: nodeSKU(n)}
		byName[n.Name] = pn
		out = append(out, pn)
	}
	var all []*planPod
	for _, p := range pods {
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed || p.Spec.NodeName == "" {
			continue
		}
		r := podRequests(p)
		kind, name := workloadOf(p)
		pp := &planPod{ns: p.Namespace, name: p.Name, kind: kind, workload: name, node: p.Spec.NodeName,
			cpu: max(r.CPUReq, 0), mem: max(r.MemReq, 0), ds: kind == "DaemonSet"}
		for _, v := range p.Spec.Volumes {
			switch {
			case v.PersistentVolumeClaim != nil:
				key := p.Namespace + "/" + v.PersistentVolumeClaim.ClaimName
				vol := pvcSize[key]
				if vol.Claim == "" {
					vol.Claim = v.PersistentVolumeClaim.ClaimName
				}
				if pv := pvOf[key]; pv != "" && attached[pv] {
					vol.Disk = true
					pp.disks = append(pp.disks, pv)
				}
				pp.pvcs = append(pp.pvcs, vol)
			case v.HostPath != nil:
				pp.pinned, pp.pinReason = true, "hostPath volume"
			}
		}
		if h := p.Spec.NodeSelector["kubernetes.io/hostname"]; h != "" {
			pp.pinned, pp.pinReason = true, "pinned to node "+h
		}
		all = append(all, pp)
		if n := byName[p.Spec.NodeName]; n != nil {
			n.usedCPU += pp.cpu
			n.usedMem += pp.mem
			n.pods = append(n.pods, pp)
		}
	}
	return out, all
}

// fits reports whether a pod can go on n without passing the headroom or the disk-attach limit.
func (n *planNode) fits(p *planPod) bool {
	if float64(n.usedCPU+p.cpu) > float64(n.allocCPU)*headroom || float64(n.usedMem+p.mem) > float64(n.allocMem)*headroom {
		return false
	}
	return n.diskLimit < 0 || n.disks+len(p.disks) <= n.diskLimit
}

func (n *planNode) take(p *planPod) {
	n.usedCPU += p.cpu
	n.usedMem += p.mem
	n.disks += len(p.disks)
}

// placeAll puts pods onto targets, biggest first, each on the node that has the least room left
// after it (best fit). Returns where each pod went, or ok=false if one doesn't fit.
func placeAll(pods []*planPod, targets []*planNode) (map[*planPod]*planNode, bool) {
	order := slices.Clone(pods)
	slices.SortFunc(order, func(a, b *planPod) int {
		if d := len(b.disks) - len(a.disks); d != 0 {
			return d // disk-carrying pods first: slots are the scarcest
		}
		return int((b.cpu + b.mem>>20) - (a.cpu + a.mem>>20))
	})
	where := map[*planPod]*planNode{}
	for _, p := range order {
		var best *planNode
		var bestSlack float64
		for _, n := range targets {
			if !n.fits(p) {
				continue
			}
			slack := (float64(n.allocCPU)*headroom-float64(n.usedCPU+p.cpu))/float64(max(n.allocCPU, 1)) +
				(float64(n.allocMem)*headroom-float64(n.usedMem+p.mem))/float64(max(n.allocMem, 1))
			if best == nil || slack < bestSlack {
				best, bestSlack = n, slack
			}
		}
		if best == nil {
			return nil, false
		}
		best.take(p)
		where[p] = best
	}
	return where, true
}

func cloneNodes(ns []*planNode) []*planNode {
	out := make([]*planNode, len(ns))
	for i, n := range ns {
		cp := *n
		out[i] = &cp
	}
	return out
}

// shrinkPlan tries to drain `remove` nodes from a pool, least loaded first and never nodes with
// pinned pods, re-placing their pods on the rest of the pool.
func shrinkPlan(cluster string, pool []*planNode, remove int, all []*planPod) (*CostPlan, bool) {
	cands := slices.Clone(pool)
	load := func(n *planNode) float64 {
		return float64(n.usedCPU)/float64(max(n.allocCPU, 1)) + float64(n.usedMem)/float64(max(n.allocMem, 1)) + float64(n.disks)*0.01
	}
	slices.SortFunc(cands, func(a, b *planNode) int { return cmpF(load(a), load(b)) })
	var drain, keep []*planNode
	for _, n := range cands {
		pinned := slices.ContainsFunc(n.pods, func(p *planPod) bool { return p.pinned && !p.ds })
		if len(drain) < remove && !pinned {
			drain = append(drain, n)
		} else {
			keep = append(keep, n)
		}
	}
	if len(drain) < remove {
		return nil, false
	}
	keep = cloneNodes(keep)
	var moving []*planPod
	for _, n := range drain {
		for _, p := range n.pods {
			if !p.ds {
				moving = append(moving, p)
			}
		}
	}
	where, ok := placeAll(moving, keep)
	if !ok {
		return nil, false
	}
	plan := &CostPlan{Type: "shrink-pool", Cluster: cluster, Pool: pool[0].pool, SKU: pool[0].sku.SKU,
		NodesFrom: len(pool), NodesTo: len(pool) - remove, Drain: []PlanDrain{}, After: []PlanNodeAfter{}, Notes: []string{}}
	for _, n := range drain {
		d := PlanDrain{Name: n.name, Disks: n.disks}
		for _, p := range n.pods {
			if !p.ds {
				d.Pods++
			}
		}
		plan.Drain = append(plan.Drain, d)
	}
	plan.Moves = groupMoves(moving, where, all)
	gains := map[*planNode]int{}
	for _, n := range where {
		gains[n]++
	}
	for _, n := range keep {
		plan.After = append(plan.After, PlanNodeAfter{Name: n.name, CPU: n.usedCPU, Mem: n.usedMem, AllocCPU: n.allocCPU, AllocMem: n.allocMem,
			Disks: n.disks, DiskLimit: n.diskLimit, Gains: gains[n]})
	}
	slices.SortFunc(plan.After, func(a, b PlanNodeAfter) int { return strings.Compare(a.Name, b.Name) })
	return plan, true
}

// groupMoves summarises moving pods per workload, with the replica count the workload has.
func groupMoves(moving []*planPod, where map[*planPod]*planNode, all []*planPod) []PlanMove {
	total := map[string]int{}
	for _, p := range all {
		total[p.ns+"/"+p.kind+"/"+p.workload]++
	}
	by := map[string]*PlanMove{}
	var keys []string
	for _, p := range moving {
		k := p.ns + "/" + p.kind + "/" + p.workload
		m := by[k]
		if m == nil {
			m = &PlanMove{Namespace: p.ns, Kind: p.kind, Name: p.workload, CPU: p.cpu, Mem: p.mem, From: []string{}, To: []string{}}
			by[k] = m
			keys = append(keys, k)
		}
		m.Replicas++
		m.CPU, m.Mem = max(m.CPU, p.cpu), max(m.Mem, p.mem)
		m.Disks += len(p.disks)
		m.From = append(m.From, p.node)
		if n := where[p]; n != nil {
			m.To = append(m.To, n.name)
		}
		m.Volumes = append(m.Volumes, p.pvcs...)
	}
	out := make([]PlanMove, 0, len(keys))
	for _, k := range keys {
		m := by[k]
		m.TotalReplicas = max(total[k], m.Replicas)
		m.From = compactCounts(m.From)
		m.To = compactCounts(m.To)
		out = append(out, *m)
	}
	slices.SortFunc(out, func(a, b PlanMove) int {
		if d := b.Disks - a.Disks; d != 0 {
			return d
		}
		return strings.Compare(a.Namespace+a.Name, b.Namespace+b.Name)
	})
	return out
}

// compactCounts turns [a a b] into ["a ×2", "b"].
func compactCounts(v []string) []string {
	n := map[string]int{}
	var order []string
	for _, s := range v {
		if n[s] == 0 {
			order = append(order, s)
		}
		n[s]++
	}
	slices.Sort(order)
	out := make([]string, 0, len(order))
	for _, s := range order {
		if n[s] > 1 {
			s = fmt.Sprintf("%s ×%d", s, n[s])
		}
		out = append(out, s)
	}
	return out
}

// poolPlans finds, per pool, the most nodes that can be drained with everything still fitting.
func (c *cluster) poolPlans(pools []CostPool) []CostPlan {
	nodes, pods := c.planState()
	byPool := map[string][]*planNode{}
	for _, n := range nodes {
		byPool[n.pool] = append(byPool[n.pool], n)
	}
	var out []CostPlan
	for _, p := range pools {
		ns := byPool[p.Name]
		floor := min(len(ns), 2)
		for remove := len(ns) - max(p.NeedReq, floor); remove >= 1; remove-- {
			plan, ok := shrinkPlan(c.name, ns, remove, pods)
			if !ok {
				continue
			}
			plan.ID = "shrink/" + c.name + "/" + p.Name
			if p.Priced {
				plan.Savings, plan.Priced = p.Hourly*hoursPerMonth*float64(remove), true
			}
			plan.Commands = shrinkCommands(c.name, p, plan)
			plan.Notes = append(plan.Notes, "Keeps 15% headroom on every remaining node and stays within each VM's disk-attach limit.")
			if slices.ContainsFunc(plan.Moves, func(m PlanMove) bool { return m.Disks > 0 }) {
				plan.Notes = append(plan.Notes, "Disks detach from the drained nodes and re-attach to the new ones; those pods restart and are briefly unavailable. Zonal disks can only follow to nodes in the same zone.")
			}
			if slices.ContainsFunc(plan.Moves, func(m PlanMove) bool { return m.Replicas == m.TotalReplicas && m.TotalReplicas == 1 }) {
				plan.Notes = append(plan.Notes, "Single-replica workloads have a short outage while they reschedule.")
			}
			out = append(out, *plan)
			break
		}
	}
	return out
}

func shrinkCommands(cluster string, p CostPool, plan *CostPlan) []string {
	var cmds []string
	for _, d := range plan.Drain {
		cmds = append(cmds, kubectl(cluster, "", "cordon "+d.Name))
	}
	for _, d := range plan.Drain {
		cmds = append(cmds, kubectl(cluster, "", "drain "+d.Name+" --ignore-daemonsets --delete-emptydir-data --timeout=10m"))
	}
	switch p.Provider {
	case "azure":
		cmds = append(cmds, fmt.Sprintf("az aks nodepool scale --cluster-name <aks-name> --resource-group <rg> --name %s --node-count %d", p.Name, plan.NodesTo))
	case "aws":
		cmds = append(cmds, fmt.Sprintf("eksctl scale nodegroup --cluster <eks-name> --name %s --nodes %d", p.Name, plan.NodesTo))
	default:
		cmds = append(cmds, fmt.Sprintf("# then reduce node pool %q to %d nodes in your provider (or its autoscaler minimum)", p.Name, plan.NodesTo))
	}
	return cmds
}

func regionOf(c CostCluster) string {
	for _, p := range c.Pools {
		if p.Region != "" {
			return p.Region
		}
	}
	return ""
}
