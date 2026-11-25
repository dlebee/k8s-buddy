package history

import (
	"testing"
	"time"
)

var spikeRules = map[string][2]SpikeRule{
	"pod":  {{Ratio: 2, Floor: 200}, {Ratio: 1.5, Floor: 256 << 20}},
	"node": {{Ratio: 2, Floor: 500}, {Ratio: 1.5, Floor: 1 << 30}},
}

// One hour of steady readings with two short CPU bursts on a pod and a memory jump on a node.
func TestSpikes(t *testing.T) {
	s := open(t)
	node := Series{Cluster: "c", Kind: "node", Name: "n1", Owner: "pool", CPULim: 4000, MemLim: 16 << 30, CPUReq: -1, MemReq: -1}
	quiet := web2
	t0 := time.Now().Add(-2 * time.Hour).Truncate(time.Hour)
	for i := range 60 {
		cpu := int64(50)
		if (i >= 10 && i < 13) || i == 40 {
			cpu = 900
		}
		nodeMem := int64(4 << 30)
		if i == 50 {
			nodeMem = 9 << 30
		}
		obs := map[Series]Sample{
			web1:  {CPU: cpu, Mem: 100 << 20},
			quiet: {CPU: 60 + int64(i%3), Mem: 200 << 20}, // small wiggles: not spikes
			node:  {CPU: 1000, Mem: nodeMem},
		}
		if err := s.Add(t0.Add(time.Duration(i)*time.Minute), obs); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Spikes("c", t0.Add(-time.Minute), spikeRules)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]SeriesSpikes{}
	for _, x := range got {
		by[x.Kind+"/"+x.Name] = x
	}
	if len(got) != 2 {
		t.Fatalf("want web-1 and n1, got %+v", got)
	}
	p := by["pod/web-1"]
	if len(p.CPU) != 2 || len(p.Mem) != 0 || p.CPUUsual != 50 || p.Owner != "Deployment/web" {
		t.Fatalf("pod: %+v", p)
	}
	if e := p.CPU[0]; e.Peak != 900 || e.Start != t0.Add(10*time.Minute).Unix() || e.End != t0.Add(13*time.Minute).Unix() {
		t.Fatalf("first burst: %+v", e)
	}
	n := by["node/n1"]
	if len(n.Mem) != 1 || len(n.CPU) != 0 || n.Mem[0].Peak != 9<<30 || n.MemLim != 16<<30 {
		t.Fatalf("node: %+v", n)
	}
}
