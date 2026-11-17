package monitor

import (
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// The cost simulator works on a snapshot of the pods and nodes running now, in memory. It never
// changes anything in a cluster: scenarios end with suggested commands for a person to run.

// planPod is one pod as the simulator sees it.
type planPod struct {
	cluster                  string
	ns, name, kind, workload string
	node                     string
	cpu, mem                 int64    // requests (summed over containers)
	memLim                   int64    // memory limit (a container without a limit counts its request)
	disks                    []string // attached disks (PV names) this pod brings along
	pvcs                     []PlanVolume
	ds, pinned               bool
	selector                 map[string]string // nodeSelector
	affinity                 bool              // has required node affinity: keep it in its pool
	tolerations              []corev1.Toleration
}

type planNode struct {
	cluster, name, pool string
	labels              map[string]string
	taints              []corev1.Taint
	allocCPU, allocMem  int64
	usedCPU, usedMem    int64 // requests of the pods on it (DaemonSets included)
	limMem              int64 // memory limits committed on it
	limCap              int64 // most memory limits it may hold (see planState)
	disks, diskLimit    int   // attached now; -1 limit unknown
	pods                []*planPod
	sku                 NodeSKU
}

type PlanVolume struct {
	Claim        string `json:"claim"`
	Size         string `json:"size"`
	StorageClass string `json:"storageClass"`
	Disk         bool   `json:"disk"` // attached to the node as a disk (counts against the VM limit)
}

type PlanDrain struct {
	Name  string `json:"name"`
	Pods  int    `json:"pods"`  // replicas the scenario moves (in the namespaces studied)
	Other int    `json:"other"` // other non-DaemonSet pods on it (system/add-ons or namespaces not studied)
	Disks int    `json:"disks"`
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
		if n.Spec.Unschedulable {
			continue // already cordoned: not a place to move pods to, and not counted as capacity
		}
		cpu, mem := n.Status.Allocatable[corev1.ResourceCPU], n.Status.Allocatable[corev1.ResourceMemory]
		d := nodeDisks(n, c.csiNode(n.Name))
		for _, dr := range d.Drivers {
			for _, v := range dr.Volumes {
				attached[v] = true
			}
		}
		pn := &planNode{cluster: c.name, name: n.Name, pool: poolOf(n), labels: n.Labels, taints: n.Spec.Taints,
			allocCPU: cpu.MilliValue(), allocMem: mem.Value(), disks: d.Attached, diskLimit: d.Limit, sku: nodeSKU(n)}
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
		pp := &planPod{cluster: c.name, ns: p.Namespace, name: p.Name, kind: kind, workload: name, node: p.Spec.NodeName,
			cpu: max(r.CPUReq, 0), mem: max(r.MemReq, 0), ds: kind == "DaemonSet", selector: p.Spec.NodeSelector,
			tolerations: p.Spec.Tolerations}
		for _, ct := range p.Spec.Containers {
			if l := quantity(ct.Resources.Limits, corev1.ResourceMemory, false); l >= 0 {
				pp.memLim += l
			} else {
				pp.memLim += max(quantity(ct.Resources.Requests, corev1.ResourceMemory, false), 0)
			}
		}
		if a := p.Spec.Affinity; a != nil && a.NodeAffinity != nil && a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
			pp.affinity = true
		}
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
				pp.pinned = true
			}
		}
		if p.Spec.NodeSelector["kubernetes.io/hostname"] != "" {
			pp.pinned = true
		}
		all = append(all, pp)
		if n := byName[p.Spec.NodeName]; n != nil {
			n.usedCPU += pp.cpu
			n.usedMem += pp.mem
			n.limMem += pp.memLim
			n.pods = append(n.pods, pp)
		}
	}
	// Memory limits are commonly overcommitted. A node may take on limits up to the highest
	// overcommit its cluster already runs with (at least 100% of allocatable), never beyond.
	ratio := 1.0
	for _, n := range out {
		if n.allocMem > 0 {
			ratio = max(ratio, float64(n.limMem)/float64(n.allocMem))
		}
	}
	for _, n := range out {
		n.limCap = max(n.limMem, int64(float64(n.allocMem)*ratio))
	}
	return out, all
}

// schedulable reports whether the pod could be scheduled on n at all: node selector, required
// affinity (kept to its own pool and cluster, conservatively) and taints.
func (n *planNode) schedulable(p *planPod, fromPool string) bool {
	for k, v := range p.selector {
		if n.labels[k] != v {
			return false
		}
	}
	if p.affinity && (n.cluster != p.cluster || n.pool != fromPool) {
		return false
	}
	for i := range n.taints {
		t := &n.taints[i]
		if t.Effect != corev1.TaintEffectNoSchedule && t.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		if !slices.ContainsFunc(p.tolerations, func(tol corev1.Toleration) bool { return tolerates(tol, t) }) {
			return false
		}
	}
	return true
}

// fits reports whether a pod can go on n: requests within the headroom, memory limits not
// overcommitted beyond what the node allows today, and a free disk-attach slot per disk.
func (n *planNode) fits(p *planPod) bool {
	if float64(n.usedCPU+p.cpu) > float64(n.allocCPU)*headroom || float64(n.usedMem+p.mem) > float64(n.allocMem)*headroom {
		return false
	}
	if n.limMem+p.memLim > n.limCap {
		return false
	}
	return n.diskLimit < 0 || n.disks+len(p.disks) <= n.diskLimit
}

func (n *planNode) take(p *planPod) {
	n.usedCPU += p.cpu
	n.usedMem += p.mem
	n.limMem += p.memLim
	n.disks += len(p.disks)
}

func cloneNodes(ns []*planNode) []*planNode {
	out := make([]*planNode, len(ns))
	for i, n := range ns {
		cp := *n
		out[i] = &cp
	}
	return out
}

// bestFit picks, among candidates, the node with the least room left after taking the pod.
func bestFit(p *planPod, fromPool string, cands []*planNode) *planNode {
	var best *planNode
	var bestSlack float64
	for _, n := range cands {
		if !n.schedulable(p, fromPool) || !n.fits(p) {
			continue
		}
		slack := (float64(n.allocCPU)*headroom-float64(n.usedCPU+p.cpu))/float64(max(n.allocCPU, 1)) +
			(float64(n.allocMem)*headroom-float64(n.usedMem+p.mem))/float64(max(n.allocMem, 1))
		if best == nil || slack < bestSlack {
			best, bestSlack = n, slack
		}
	}
	return best
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

// tolerates: the Kubernetes toleration rules for one taint (key/operator/value/effect).
func tolerates(tol corev1.Toleration, t *corev1.Taint) bool {
	if tol.Effect != "" && tol.Effect != t.Effect {
		return false
	}
	if tol.Key == "" {
		return tol.Operator == corev1.TolerationOpExists
	}
	if tol.Key != t.Key {
		return false
	}
	return tol.Operator == corev1.TolerationOpExists || tol.Value == t.Value
}
