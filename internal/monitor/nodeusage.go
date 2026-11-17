package monitor

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
)

// Live node usage (metrics API) against allocatable, plus what the pods scheduled on each node
// request. On node rows RowUsage reads: CPU/Mem = used now, CPUReq/MemReq = summed pod requests,
// CPULim/MemLim = allocatable.

// podHoldsResources reports whether a pod still counts against its node (as the scheduler sees it).
func podHoldsResources(p *corev1.Pod) bool {
	return p.Spec.NodeName != "" && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed
}

// nodeRequests sums container requests and limits of the pods holding resources on each node.
// Unset values count as zero; a node with no pods has no entry.
func nodeRequests(pods []*corev1.Pod) (req, lim map[string]ResourceNumbers) {
	req, lim = map[string]ResourceNumbers{}, map[string]ResourceNumbers{}
	for _, p := range pods {
		if !podHoldsResources(p) {
			continue
		}
		u := podRequests(p)
		r, l := req[p.Spec.NodeName], lim[p.Spec.NodeName]
		r.CPU += max(u.CPUReq, 0)
		r.Memory += max(u.MemReq, 0)
		l.CPU += max(u.CPULim, 0)
		l.Memory += max(u.MemLim, 0)
		req[p.Spec.NodeName], lim[p.Spec.NodeName] = r, l
	}
	return req, lim
}

// nodeRowUsage builds a node row's usage: requests from its pods (-1 when pods aren't synced yet),
// allocatable as the ceiling, live usage unknown until metrics fill it in.
func nodeRowUsage(n *corev1.Node, req map[string]ResourceNumbers, podsKnown bool) *RowUsage {
	cpu, mem := n.Status.Allocatable[corev1.ResourceCPU], n.Status.Allocatable[corev1.ResourceMemory]
	u := &RowUsage{CPU: -1, Mem: -1, CPUReq: -1, MemReq: -1, CPULim: cpu.MilliValue(), MemLim: mem.Value()}
	if podsKnown {
		r := req[n.Name]
		u.CPUReq, u.MemReq = r.CPU, r.Memory
	}
	return u
}

// nodeMetricsByCluster fetches live node usage for the clusters in scope (cached, in parallel).
// Clusters without a metrics API are left out.
func (m *Manager) nodeMetricsByCluster(scope []string) map[string]map[string]ResourceNumbers {
	out := map[string]map[string]ResourceNumbers{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range m.connected() {
		if !inScope(scope, c.name) || !c.synced("nodes") {
			continue
		}
		wg.Go(func() {
			if nodes, err := c.allNodeMetrics(); err == nil {
				mu.Lock()
				out[c.name] = nodes
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return out
}

type NodeUsageReport struct {
	Metrics     bool             `json:"metrics"`
	MetricsErr  string           `json:"metricsError,omitempty"`
	Window      string           `json:"window,omitempty"`
	Timestamp   time.Time        `json:"timestamp,omitzero"`
	Usage       *ResourceNumbers `json:"usage,omitempty"`
	Allocatable ResourceNumbers  `json:"allocatable"`
	Capacity    ResourceNumbers  `json:"capacity"`
	// Requests and Limits sum the containers of pods holding resources on the node.
	Requests ResourceNumbers `json:"requests"`
	Limits   ResourceNumbers `json:"limits"`
	Pods     int             `json:"pods"`
}

// NodeUsage reports a node's live usage (read fresh from the metrics API) against allocatable,
// and what its pods request.
func (m *Manager) NodeUsage(clusterName, name string) (NodeUsageReport, error) {
	var rep NodeUsageReport
	c, err := m.cluster(clusterName)
	if err != nil {
		return rep, err
	}
	n, err := c.nodes.Get(name)
	if err != nil {
		return rep, err
	}
	rn := func(rl corev1.ResourceList) ResourceNumbers {
		cpu, mem := rl[corev1.ResourceCPU], rl[corev1.ResourceMemory]
		return ResourceNumbers{CPU: cpu.MilliValue(), Memory: mem.Value()}
	}
	rep.Allocatable, rep.Capacity = rn(n.Status.Allocatable), rn(n.Status.Capacity)
	var onNode []*corev1.Pod
	if pods, err := c.pods.List(labels.Everything()); err == nil {
		for _, p := range pods {
			if p.Spec.NodeName == name && podHoldsResources(p) {
				onNode = append(onNode, p)
			}
		}
	}
	req, lim := nodeRequests(onNode)
	rep.Requests, rep.Limits, rep.Pods = req[name], lim[name], len(onNode)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := c.cs.CoreV1().RESTClient().Get().AbsPath("/apis/metrics.k8s.io/v1beta1/nodes", name).DoRaw(ctx)
	if err != nil {
		rep.MetricsErr = truncate(metricsErr(err).Error(), 200)
		return rep, nil
	}
	var it struct {
		Timestamp time.Time                    `json:"timestamp"`
		Window    string                       `json:"window"`
		Usage     struct{ CPU, Memory string } `json:"usage"`
	}
	if err := json.Unmarshal(raw, &it); err != nil {
		rep.MetricsErr = truncate(err.Error(), 200)
		return rep, nil
	}
	cpu, _ := resource.ParseQuantity(firstNonEmpty(it.Usage.CPU, "0"))
	mem, _ := resource.ParseQuantity(firstNonEmpty(it.Usage.Memory, "0"))
	rep.Metrics, rep.Window, rep.Timestamp = true, it.Window, it.Timestamp
	rep.Usage = &ResourceNumbers{CPU: cpu.MilliValue(), Memory: mem.Value()}
	return rep, nil
}
