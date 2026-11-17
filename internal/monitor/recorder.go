package monitor

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/dlebee/k8s-buddy/internal/history"
)

// The recorder samples node and pod CPU/memory once a minute from the metrics API and keeps it in
// the local history store, so drawers can graph usage and cost optimization can look at peaks.

const sampleEvery = time.Minute

// UsageHistory reports whether recording is on and gives access to the store (nil when disabled
// or unavailable).
func (m *Manager) History() *history.Store {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hist
}

// RunRecorder samples until ctx ends. Settings are re-read every tick, so turning history off or
// changing the retention takes effect within a minute.
func (m *Manager) RunRecorder(ctx context.Context, h *history.Store) {
	m.mu.Lock()
	m.hist = h
	m.mu.Unlock()
	t := time.NewTicker(sampleEvery)
	defer t.Stop()
	lastPrune := time.Time{}
	for {
		s := m.settings.Get()
		if s.UsageHistory {
			m.recordOnce(h)
			if time.Since(lastPrune) > time.Hour {
				if err := h.Prune(time.Duration(s.UsageRetentionDays) * 24 * time.Hour); err != nil {
					m.log.Warn("usage history prune", "err", err)
				}
				lastPrune = time.Now()
			}
		}
		select {
		case <-ctx.Done():
			_ = h.Flush()
			return
		case <-t.C:
		}
	}
}

func (m *Manager) recordOnce(h *history.Store) {
	now := time.Now()
	obs := map[history.Series]history.Sample{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range m.connected() {
		if !c.synced("pods") || !c.synced("nodes") {
			continue
		}
		wg.Go(func() {
			part := c.usageSamples()
			mu.Lock()
			for k, v := range part {
				obs[k] = v
			}
			mu.Unlock()
		})
	}
	wg.Wait()
	if len(obs) == 0 {
		return
	}
	if err := h.Add(now, obs); err != nil {
		m.log.Warn("usage history write", "err", err)
	}
}

// usageSamples collects one observation per running pod and per node of this cluster.
func (c *cluster) usageSamples() map[history.Series]history.Sample {
	out := map[history.Series]history.Sample{}
	if pm, err := c.allPodMetrics(); err == nil {
		pods, _ := c.pods.List(labels.Everything())
		for _, p := range pods {
			u, ok := pm[p.Namespace+"/"+p.Name]
			if !ok {
				continue
			}
			r := podRequests(p)
			kind, name := workloadOf(p)
			out[history.Series{Cluster: c.name, Kind: "pod", Namespace: p.Namespace, Name: p.Name, Owner: kind + "/" + name,
				CPUReq: r.CPUReq, CPULim: r.CPULim, MemReq: r.MemReq, MemLim: r.MemLim}] = history.Sample{CPU: u.CPU, Mem: u.Memory}
		}
	}
	if nm, err := c.allNodeMetrics(); err == nil {
		nodes, _ := c.nodes.List(labels.Everything())
		for _, n := range nodes {
			u, ok := nm[n.Name]
			if !ok {
				continue
			}
			cpu, mem := n.Status.Allocatable[corev1.ResourceCPU], n.Status.Allocatable[corev1.ResourceMemory]
			out[history.Series{Cluster: c.name, Kind: "node", Name: n.Name, Owner: poolOf(n),
				CPUReq: -1, MemReq: -1, CPULim: cpu.MilliValue(), MemLim: mem.Value()}] = history.Sample{CPU: u.CPU, Mem: u.Memory}
		}
	}
	return out
}

// allNodeMetrics reads every node's current usage in one call.
func (c *cluster) allNodeMetrics() (map[string]ResourceNumbers, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := c.cs.CoreV1().RESTClient().Get().AbsPath("/apis/metrics.k8s.io/v1beta1/nodes").DoRaw(ctx)
	if err != nil {
		return nil, metricsErr(err)
	}
	var l struct {
		Items []struct {
			Metadata struct{ Name string }        `json:"metadata"`
			Usage    struct{ CPU, Memory string } `json:"usage"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, err
	}
	out := make(map[string]ResourceNumbers, len(l.Items))
	for _, it := range l.Items {
		cpu, _ := resource.ParseQuantity(firstNonEmpty(it.Usage.CPU, "0"))
		mem, _ := resource.ParseQuantity(firstNonEmpty(it.Usage.Memory, "0"))
		out[it.Metadata.Name] = ResourceNumbers{CPU: cpu.MilliValue(), Memory: mem.Value()}
	}
	return out, nil
}
