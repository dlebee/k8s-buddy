package monitor

import (
	"math"
	"testing"

	"github.com/dlebee/k8s-buddy/internal/history"
)

// Pods of one workload fold into one row per resource; the biggest peak sets usual and limit.
func TestSpikeRows(t *testing.T) {
	ev := func(start, mins int64, peak float64) history.SpikeEvent {
		return history.SpikeEvent{Start: start, End: start + mins*60, Peak: peak, PeakAt: start}
	}
	series := []history.SeriesSpikes{
		{Kind: "pod", Namespace: "shop", Name: "web-1", Owner: "Deployment/web", CPULim: 1000, CPUUsual: 50, CPU: []history.SpikeEvent{ev(1000, 3, 600)}},
		{Kind: "pod", Namespace: "shop", Name: "web-2", Owner: "Deployment/web", CPULim: 1000, CPUUsual: 80, CPU: []history.SpikeEvent{ev(5000, 1, 900), ev(9000, 2, 400)}},
		{Kind: "pod", Namespace: "kube-system", Name: "dns-1", Owner: "Deployment/dns", MemLim: -1, MemUsual: 100 << 20, Mem: []history.SpikeEvent{ev(2000, 5, 500<<20)}},
		{Kind: "pod", Namespace: "shop", Name: "job-x", MemLim: -1, MemUsual: 1 << 20, Mem: []history.SpikeEvent{ev(3000, 1, 400<<20)}},
		{Kind: "node", Name: "n1", Owner: "pool", MemLim: 16 << 30, MemUsual: 4 << 30, Mem: []history.SpikeEvent{ev(4000, 10, 9<<30)}},
	}
	rows := spikeRows("c", series)
	by := map[string]SpikeRow{}
	for _, r := range rows {
		by[r.Kind+"/"+r.Name+"/"+r.Resource] = r
	}
	if len(rows) != 4 {
		t.Fatalf("rows: %+v", rows)
	}
	if w := by["Deployment/web/cpu"]; w.Pods != 2 || w.Spikes != 3 || w.Minutes != 6 || w.Peak != 900 || w.Usual != 80 || w.LastAt != 9000 || w.HistoryKind != "workload" || w.HistoryName != "Deployment/web" || w.PeakPod != "web-2" || w.System {
		t.Fatalf("web: %+v", w)
	}
	if d := by["Deployment/dns/memory"]; !d.System || d.Limit != -1 {
		t.Fatalf("dns: %+v", d)
	}
	if p := by["Pod/job-x/memory"]; p.HistoryKind != "pod" || p.HistoryName != "job-x" {
		t.Fatalf("bare pod: %+v", p)
	}
	if n := by["Node/n1/memory"]; n.Pods != 0 || n.Limit != 16<<30 || n.HistoryKind != "node" || n.Namespace != "" {
		t.Fatalf("node: %+v", n)
	}
}

func TestSpikeLimitsClamp(t *testing.T) {
	got := SpikeLimits{NodeCPU: 0, NodeMem: math.NaN(), PodCPU: 500, PodMem: 0.2}.clamp()
	if got != (SpikeLimits{NodeCPU: 50, NodeMem: 50, PodCPU: 100, PodMem: 1}) {
		t.Fatalf("%+v", got)
	}
}
