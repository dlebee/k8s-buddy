package monitor

import (
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/dlebee/k8s-buddy/internal/history"
)

// Cost optimization: how much of each node pool's capacity is requested and actually used, how
// many nodes the pool would need, which workloads ask for far more (or less) than they use, and
// which tagged clusters could fold into another. Everything is advice; nothing is changed.

const (
	hoursPerMonth = 730
	headroom      = 0.85 // fill nodes to at most 85% of what's left after DaemonSets
)

type CostPool struct {
	Name     string  `json:"name"`
	SKU      string  `json:"sku"`
	Region   string  `json:"region"`
	Provider string  `json:"provider"`
	Mode     string  `json:"mode,omitempty"` // AKS system/user
	Nodes    int     `json:"nodes"`
	NodeCPU  int64   `json:"nodeCpu"` // allocatable per node (smallest node)
	NodeMem  int64   `json:"nodeMem"`
	DSCPU    int64   `json:"dsCpu"` // DaemonSet requests per node (busiest node)
	DSMem    int64   `json:"dsMem"`
	ReqCPU   int64   `json:"reqCpu"` // other pods' requests, whole pool
	ReqMem   int64   `json:"reqMem"`
	UseCPU   float64 `json:"useCpu"` // sum of pods' p95 usage (requests where no history)
	UseMem   float64 `json:"useMem"`
	Disks    int     `json:"disks"`
	DiskMax  int     `json:"diskLimit"` // per node, -1 unknown
	NeedReq  int     `json:"needByRequests"`
	NeedUse  int     `json:"needByUsage"`
	Binding  string  `json:"binding"` // what sets NeedReq: cpu, memory, disks, minimum
	Hourly   float64 `json:"hourlyPerNode"`
	Priced   bool    `json:"priced"`
	Source   string  `json:"priceSource,omitempty"`
	Monthly  float64 `json:"monthly"`
	Savings  float64 `json:"savings"`        // dropping to NeedReq
	SavingsU float64 `json:"savingsByUsage"` // dropping to NeedUse (after right-sizing)
}

type RightSize struct {
	Namespace string  `json:"namespace"`
	Kind      string  `json:"kind"`
	Name      string  `json:"name"`
	Pods      int     `json:"pods"`
	ReqCPU    int64   `json:"reqCpu"` // per pod
	ReqMem    int64   `json:"reqMem"`
	P95CPU    float64 `json:"p95Cpu"` // per pod, busiest pod
	P95Mem    float64 `json:"p95Mem"`
	MaxCPU    float64 `json:"maxCpu"`
	MaxMem    float64 `json:"maxMem"`
	SugCPU    int64   `json:"suggestCpu"`
	SugMem    int64   `json:"suggestMem"`
	Direction string  `json:"direction"` // over | under
	FreeCPU   int64   `json:"freedCpu"`  // whole workload
	FreeMem   int64   `json:"freedMem"`
}

type CostCluster struct {
	Cluster      string      `json:"cluster"`
	Provider     string      `json:"provider"`
	Tags         []string    `json:"tags"`
	HistoryHours float64     `json:"historyHours"`
	Pools        []CostPool  `json:"pools"`
	Workloads    []RightSize `json:"workloads"`
	Nodes        int         `json:"nodes"`
	Priced       bool        `json:"priced"` // every node has a price
	Monthly      float64     `json:"monthly"`
	Savings      float64     `json:"savings"`
	SavingsU     float64     `json:"savingsByUsage"`
}

type Consolidation struct {
	Tag         string   `json:"tag"`
	From        string   `json:"from"`
	Into        string   `json:"into"`
	MoveCPU     int64    `json:"moveCpu"`
	MoveMem     int64    `json:"moveMem"`
	SpareCPU    int64    `json:"spareCpu"`
	SpareMem    int64    `json:"spareMem"`
	Workloads   int      `json:"workloads"`
	Already     int      `json:"alreadyThere"`
	WithVolumes []string `json:"withVolumes"`
	Pinned      []string `json:"pinned"`
	Savings     float64  `json:"savings"`
	Priced      bool     `json:"priced"`
}

type CostReport struct {
	Generated      time.Time       `json:"generated"`
	WindowHours    int             `json:"windowHours"`
	History        bool            `json:"history"`
	Clusters       []CostCluster   `json:"clusters"`
	Consolidations []Consolidation `json:"consolidations"`
}

// systemNamespace: platform pieces every cluster runs for itself; not worth moving between clusters.
func systemNamespace(ns string) bool {
	return strings.HasPrefix(ns, "kube-") || strings.HasSuffix(ns, "-system") || ns == "gatekeeper-system" || ns == "tigera-operator"
}

func (m *Manager) CostReport(windowHours int) CostReport {
	s := m.settings.Get()
	h := m.History()
	rep := CostReport{Generated: time.Now(), WindowHours: windowHours, History: h != nil && s.UsageHistory,
		Clusters: []CostCluster{}, Consolidations: []Consolidation{}}
	since := time.Now().Add(-time.Duration(windowHours) * time.Hour)
	type full struct {
		cc   CostCluster
		work map[string]wlFootprint
		c    *cluster
	}
	var mu sync.Mutex
	var all []full
	var wg sync.WaitGroup
	for _, c := range m.connected() {
		if !c.synced("pods") || !c.synced("nodes") {
			continue
		}
		wg.Go(func() {
			var peaks map[string]history.Peak
			var hours float64
			if rep.History {
				peaks, hours, _ = h.PodPeaks(c.name, since)
			}
			cc, work := c.costCluster(peaks, hours)
			cc.Tags = s.ClusterTags[c.name]
			mu.Lock()
			all = append(all, full{cc, work, c})
			mu.Unlock()
		})
	}
	wg.Wait()
	slices.SortFunc(all, func(a, b full) int { return strings.Compare(a.cc.Cluster, b.cc.Cluster) })
	for _, f := range all {
		rep.Clusters = append(rep.Clusters, f.cc)
	}
	// Clusters sharing a tag: could one's workloads fit in another's spare capacity?
	byTag := map[string][]int{}
	for i, f := range all {
		for _, t := range f.cc.Tags {
			byTag[t] = append(byTag[t], i)
		}
	}
	for tag, idx := range byTag {
		if len(idx) < 2 {
			continue
		}
		for _, a := range idx {
			var best *Consolidation
			for _, b := range idx {
				if a == b {
					continue
				}
				con := consolidate(tag, all[a].cc, all[a].work, all[b].cc, all[b].work)
				if con != nil && (best == nil || con.SpareCPU > best.SpareCPU) {
					best = con
				}
			}
			if best != nil {
				rep.Consolidations = append(rep.Consolidations, *best)
			}
		}
	}
	slices.SortFunc(rep.Consolidations, func(a, b Consolidation) int { return int(b.Savings - a.Savings) })
	return rep
}

// wlFootprint is one workload's requests in a cluster, for cross-cluster moves.
type wlFootprint struct {
	cpu, mem int64
	volumes  bool
	pinned   bool
	system   bool
}

func (c *cluster) costCluster(peaks map[string]history.Peak, hours float64) (CostCluster, map[string]wlFootprint) {
	cc := CostCluster{Cluster: c.name, HistoryHours: math.Round(hours*10) / 10, Pools: []CostPool{}, Workloads: []RightSize{}, Priced: true}
	nodes, _ := c.nodes.List(labels.Everything())
	pods, _ := c.pods.List(labels.Everything())
	poolOfNode := map[string]string{}
	pools := map[string]*CostPool{}
	dsOnNode := map[string][2]int64{}
	disksOnNode := map[string]int{}
	for _, n := range nodes {
		name := poolOf(n)
		poolOfNode[n.Name] = name
		p := pools[name]
		sku := nodeSKU(n)
		if p == nil {
			p = &CostPool{Name: name, SKU: sku.SKU, Region: sku.Region, Provider: sku.Provider, Mode: n.Labels["kubernetes.azure.com/mode"], DiskMax: -1}
			pools[name] = p
		}
		cc.Provider = firstNonEmpty(cc.Provider, sku.Provider)
		p.Nodes++
		cpu, mem := n.Status.Allocatable[corev1.ResourceCPU], n.Status.Allocatable[corev1.ResourceMemory]
		if p.NodeCPU == 0 || cpu.MilliValue() < p.NodeCPU {
			p.NodeCPU = cpu.MilliValue()
		}
		if p.NodeMem == 0 || mem.Value() < p.NodeMem {
			p.NodeMem = mem.Value()
		}
		d := nodeDisks(n, c.csiNode(n.Name))
		disksOnNode[n.Name] = d.Attached
		p.Disks += d.Attached
		if d.Limit >= 0 && (p.DiskMax < 0 || d.Limit < p.DiskMax) {
			p.DiskMax = d.Limit
		}
	}
	type wl struct {
		ns, kind, name string
		pods           int
		reqCPU, reqMem int64
		p95CPU, p95Mem float64
		maxCPU, maxMem float64
		known          int
		volumes        bool
		pinned         bool
	}
	work := map[string]*wl{}
	items := map[string][]packItem{} // pool → its non-DaemonSet pods, for bin packing
	for _, p := range pods {
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		r := podRequests(p)
		cpu, mem := max(r.CPUReq, 0), max(r.MemReq, 0)
		kind, name := workloadOf(p)
		pool := poolOfNode[p.Spec.NodeName]
		pk, hasPeak := peaks[p.Namespace+"/"+p.Name]
		if kind == "DaemonSet" {
			v := dsOnNode[p.Spec.NodeName]
			dsOnNode[p.Spec.NodeName] = [2]int64{v[0] + cpu, v[1] + mem}
		} else if pp := pools[pool]; pp != nil {
			pp.ReqCPU += cpu
			pp.ReqMem += mem
			if hasPeak {
				pp.UseCPU += pk.CPU95
				pp.UseMem += pk.Mem95
			} else {
				pp.UseCPU += float64(cpu)
				pp.UseMem += float64(mem)
			}
			it := packItem{cpu: float64(cpu), mem: float64(mem), ucpu: float64(cpu), umem: float64(mem)}
			if hasPeak {
				it.ucpu, it.umem = pk.CPU95, pk.Mem95
			}
			items[pool] = append(items[pool], it)
		}
		key := p.Namespace + "/" + kind + "/" + name
		w := work[key]
		if w == nil {
			w = &wl{ns: p.Namespace, kind: kind, name: name}
			work[key] = w
		}
		w.pods++
		w.reqCPU, w.reqMem = max(w.reqCPU, cpu), max(w.reqMem, mem)
		if hasPeak && pk.Buckets >= 12 { // at least an hour of samples
			w.known++
			w.p95CPU, w.p95Mem = max(w.p95CPU, pk.CPU95), max(w.p95Mem, pk.Mem95)
			w.maxCPU, w.maxMem = max(w.maxCPU, pk.CPUMax), max(w.maxMem, pk.MemMax)
		}
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				w.volumes = true
			}
			if v.HostPath != nil {
				w.pinned = true
			}
		}
		if pinnedPod(p) {
			w.pinned = true
		}
	}
	for n, v := range dsOnNode {
		if p := pools[poolOfNode[n]]; p != nil {
			p.DSCPU, p.DSMem = max(p.DSCPU, v[0]), max(p.DSMem, v[1])
		}
	}
	for name, p := range pools {
		capCPU := float64(p.NodeCPU-p.DSCPU) * headroom
		capMem := float64(p.NodeMem-p.DSMem) * headroom
		floor := min(p.Nodes, 2) // never suggest a single node for a pool that has redundancy today
		need := func(byUsage bool) (int, string) {
			n, why := floor, "minimum"
			if b, bind := pack(items[name], capCPU, capMem, byUsage); b > n {
				n, why = b, bind
			}
			if p.DiskMax > 0 {
				if d := int(math.Ceil(float64(p.Disks) / float64(p.DiskMax))); d > n {
					n, why = d, "disks"
				}
			}
			return min(n, p.Nodes), why
		}
		p.NeedReq, p.Binding = need(false)
		p.NeedUse, _ = need(true)
		p.NeedUse = min(p.NeedUse, p.NeedReq)
		if hr, src, ok := NodePrice(NodeSKU{p.Provider, p.SKU, p.Region}); ok {
			p.Hourly, p.Priced, p.Source = hr, true, src
			p.Monthly = hr * hoursPerMonth * float64(p.Nodes)
			p.Savings = hr * hoursPerMonth * float64(p.Nodes-p.NeedReq)
			p.SavingsU = hr * hoursPerMonth * float64(p.Nodes-p.NeedUse)
		} else {
			cc.Priced = false
		}
		cc.Nodes += p.Nodes
		cc.Monthly += p.Monthly
		cc.Savings += p.Savings
		cc.SavingsU += p.SavingsU
		cc.Pools = append(cc.Pools, *p)
	}
	slices.SortFunc(cc.Pools, func(a, b CostPool) int { return strings.Compare(a.Name, b.Name) })
	foot := map[string]wlFootprint{}
	for key, w := range work {
		foot[key] = wlFootprint{cpu: w.reqCPU * int64(w.pods), mem: w.reqMem * int64(w.pods), volumes: w.volumes, pinned: w.pinned, system: systemNamespace(w.ns) || w.kind == "DaemonSet"}
		if w.known == 0 || w.kind == "Pod" {
			continue
		}
		rs := RightSize{Namespace: w.ns, Kind: w.kind, Name: w.name, Pods: w.pods, ReqCPU: w.reqCPU, ReqMem: w.reqMem,
			P95CPU: w.p95CPU, P95Mem: w.p95Mem, MaxCPU: w.maxCPU, MaxMem: w.maxMem}
		// Suggested requests: 30% above the 95th percentile, and memory never below the observed peak
		// plus 15% (memory requests that are too low get pods evicted or OOM-killed).
		rs.SugCPU = roundUp(int64(w.p95CPU*1.3), 10)
		rs.SugMem = roundUp(int64(max(w.p95Mem*1.3, w.maxMem*1.15)), 16<<20)
		overCPU := w.reqCPU-rs.SugCPU >= 100 && float64(w.reqCPU) > w.p95CPU*2
		overMem := w.reqMem-rs.SugMem >= 256<<20 && float64(w.reqMem) > w.maxMem*1.5
		underMem := w.reqMem > 0 && w.p95Mem > float64(w.reqMem)*1.1
		underCPU := w.reqCPU > 0 && w.p95CPU > float64(w.reqCPU)*1.5
		switch {
		case underMem || underCPU:
			rs.Direction = "under"
		case overCPU || overMem:
			rs.Direction = "over"
			if overCPU {
				rs.FreeCPU = (w.reqCPU - rs.SugCPU) * int64(w.pods)
			}
			if overMem {
				rs.FreeMem = (w.reqMem - rs.SugMem) * int64(w.pods)
			}
		default:
			continue
		}
		cc.Workloads = append(cc.Workloads, rs)
	}
	slices.SortFunc(cc.Workloads, func(a, b RightSize) int {
		if a.Direction != b.Direction {
			return strings.Compare(b.Direction, a.Direction) // under first: those are risks
		}
		return int((b.FreeCPU/100 + b.FreeMem>>27) - (a.FreeCPU/100 + a.FreeMem>>27))
	})
	if len(cc.Workloads) > 40 {
		cc.Workloads = cc.Workloads[:40]
	}
	return cc, foot
}

type packItem struct{ cpu, mem, ucpu, umem float64 }

// pack places pods onto identical nodes, biggest first (first-fit decreasing), and returns how
// many nodes it took and which resource filled them. A pod too big for one node's room gets a node
// to itself (the scheduler would still need somewhere to put it).
func pack(items []packItem, capCPU, capMem float64, byUsage bool) (int, string) {
	if capCPU <= 0 || capMem <= 0 {
		return 0, "minimum"
	}
	type bin struct{ cpu, mem float64 }
	size := func(it packItem) (float64, float64) {
		if byUsage {
			return it.ucpu, it.umem
		}
		return it.cpu, it.mem
	}
	sorted := slices.Clone(items)
	slices.SortFunc(sorted, func(a, b packItem) int {
		ac, am := size(a)
		bc, bm := size(b)
		return cmpF(max(bc/capCPU, bm/capMem), max(ac/capCPU, am/capMem))
	})
	var bins []bin
	for _, it := range sorted {
		c, m := size(it)
		placed := false
		for i := range bins {
			if bins[i].cpu+c <= capCPU && bins[i].mem+m <= capMem {
				bins[i].cpu += c
				bins[i].mem += m
				placed = true
				break
			}
		}
		if !placed {
			bins = append(bins, bin{c, m})
		}
	}
	var cpu, mem float64
	for _, b := range bins {
		cpu, mem = cpu+b.cpu, mem+b.mem
	}
	if cpu/capCPU >= mem/capMem {
		return len(bins), "cpu"
	}
	return len(bins), "memory"
}

func cmpF(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func roundUp(v, step int64) int64 { return max(step, (v+step-1)/step*step) }

// pinnedPod: required node affinity or a node selector beyond OS/arch ties it to specific nodes.
func pinnedPod(p *corev1.Pod) bool {
	for k := range p.Spec.NodeSelector {
		if k != "kubernetes.io/os" && k != "kubernetes.io/arch" {
			return true
		}
	}
	a := p.Spec.Affinity
	return a != nil && a.NodeAffinity != nil && a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil
}

// consolidate checks whether `from`'s movable workloads fit in `into`'s spare capacity. Workloads
// that already run in `into` (same namespace, kind and name) don't need to move.
func consolidate(tag string, from CostCluster, fw map[string]wlFootprint, into CostCluster, iw map[string]wlFootprint) *Consolidation {
	con := Consolidation{Tag: tag, From: from.Cluster, Into: into.Cluster, WithVolumes: []string{}, Pinned: []string{}}
	for key, w := range fw {
		if w.system {
			continue
		}
		if _, ok := iw[key]; ok {
			con.Already++
			continue
		}
		con.Workloads++
		con.MoveCPU += w.cpu
		con.MoveMem += w.mem
		if w.volumes {
			con.WithVolumes = append(con.WithVolumes, key)
		}
		if w.pinned {
			con.Pinned = append(con.Pinned, key)
		}
	}
	for _, p := range into.Pools {
		con.SpareCPU += max(0, int64(float64(p.NodeCPU-p.DSCPU)*headroom)*int64(p.Nodes)-p.ReqCPU)
		con.SpareMem += max(0, int64(float64(p.NodeMem-p.DSMem)*headroom)*int64(p.Nodes)-p.ReqMem)
	}
	if con.Workloads == 0 || con.MoveCPU > con.SpareCPU || con.MoveMem > con.SpareMem {
		return nil
	}
	slices.Sort(con.WithVolumes)
	slices.Sort(con.Pinned)
	con.Savings, con.Priced = from.Monthly, from.Priced
	return &con
}
