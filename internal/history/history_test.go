package history

import (
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var web1 = Series{Cluster: "c", Kind: "pod", Namespace: "ns", Name: "web-1", Owner: "Deployment/web", CPUReq: 100, MemReq: 1 << 20, CPULim: -1, MemLim: -1}
var web2 = Series{Cluster: "c", Kind: "pod", Namespace: "ns", Name: "web-2", Owner: "Deployment/web", CPUReq: 100, MemReq: 1 << 20, CPULim: -1, MemLim: -1}

func TestBlockRoundTripWithGaps(t *testing.T) {
	for _, tier := range []int{tier1, tier5, tier15} {
		b := block{0: {N: 1, CPU: 40, CPUMin: 40, CPUMax: 40, Mem: 1000, MemMin: 1000, MemMax: 1000}, 3: {N: 1, CPU: 7, CPUMin: 7, CPUMax: 7, Mem: 990, MemMin: 990, MemMax: 990}}
		if tier != tier1 {
			b[1] = bucket{N: 4, CPU: 50, CPUMin: 3, CPUMax: 900, Mem: 1200, MemMin: 1100, MemMax: 1300}
		}
		got, err := decodeBlock(tier, encodeBlock(tier, b))
		if err != nil || len(got) != len(b) {
			t.Fatalf("tier %d: %v %v", tier, got, err)
		}
		for i, v := range b {
			if got[i] != v {
				t.Fatalf("tier %d slot %d: %+v != %+v", tier, i, got[i], v)
			}
		}
	}
	if len(encodeBlock(tier1, block{0: {CPU: 40, Mem: 1000}, 1: {CPU: 41, Mem: 1000}})) > 8+2+3+2 {
		t.Fatal("steady values should pack into a few bytes")
	}
}

func TestPageSize(t *testing.T) {
	s := open(t)
	var ps int
	if err := s.db.QueryRow(`PRAGMA page_size`).Scan(&ps); err != nil || ps != 16384 {
		t.Fatalf("page size %d %v", ps, err)
	}
}

func TestOneMinuteReadingsAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-30 * time.Minute).Truncate(time.Minute)
	for i := range 3 {
		if err := s.Add(t0.Add(time.Duration(i)*time.Minute), map[Series]Sample{web1: {CPU: int64(10 * (i + 1)), Mem: 100 << 10}, web2: {CPU: 5, Mem: 50 << 10}}); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	// Restarted within the hour: earlier minutes are kept, new ones added.
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Add(t0.Add(3*time.Minute), map[Series]Sample{web1: {CPU: 99, Mem: 100 << 10}}); err != nil {
		t.Fatal(err)
	}
	pts, info, err := s.Query("c", "pod", "ns", "web-1", t0.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 4 || pts[0].Step != 60 || pts[0].CPU != 10 || pts[3].CPU != 99 || pts[0].Mem != 100<<10 || info.CPUReq != 100 {
		t.Fatalf("pod: %+v %+v", pts, info)
	}
	w, wi, err := s.Query("c", "workload", "ns", "Deployment/web", t0.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(w) != 4 || w[0].CPU != 15 || wi.Pods != 2 {
		t.Fatalf("workload: %+v %+v", w, wi)
	}
}

// One hour of 1-minute readings, then aged: 5-minute buckets at 6 hours, exact 15-minute buckets
// kept after the 5-minute ones go at 2 days, everything gone after the retention.
func TestRollupTiers(t *testing.T) {
	s := open(t)
	hour := time.Now().Add(-10 * time.Hour).Truncate(time.Hour)
	// First quarter: minutes 0–4 = 1,1,1,2,2  5–9 = 1,1,1,2,2  10–14 = 9,9,9,9,9.
	// Median of the 5-minute medians (1, 1, 9) would be 1; the exact median of the 15 is 2.
	q := []int64{1, 1, 1, 2, 2, 1, 1, 1, 2, 2, 9, 9, 9, 9, 9}
	for i := range 60 {
		if i == 20 {
			continue // a gap: not counted
		}
		v := int64(50)
		if i < 15 {
			v = q[i]
		}
		if err := s.Add(hour.Add(time.Duration(i)*time.Minute), map[Series]Sample{web1: {CPU: v, Mem: v << 10}}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	if err := s.prune(now, 14*day); err != nil {
		t.Fatal(err)
	}
	pts, _, err := s.Query("c", "pod", "ns", "web-1", hour.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 12 || pts[0].Step != 300 || pts[0].N != 5 || pts[0].CPU != 1 || pts[0].CPUMin != 1 || pts[0].CPUMax != 2 || pts[2].CPU != 9 || pts[4].N != 4 {
		t.Fatalf("5-minute buckets: %+v", pts)
	}
	// Three days later the 5-minute buckets are gone; the 15-minute ones remain, exact.
	if err := s.prune(now.Add(3*day), 14*day); err != nil {
		t.Fatal(err)
	}
	pts, _, err = s.Query("c", "pod", "ns", "web-1", hour.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 4 || pts[0].Step != 900 || pts[0].N != 15 || pts[0].CPU != 2 || pts[0].CPUMin != 1 || pts[0].CPUMax != 9 || pts[1].N != 14 {
		t.Fatalf("15-minute buckets: %+v", pts)
	}
	// Past the retention: nothing left.
	if err := s.prune(now.Add(20*day), 14*day); err != nil {
		t.Fatal(err)
	}
	if pts, _, _ = s.Query("c", "pod", "ns", "web-1", hour.Add(-time.Minute)); len(pts) != 0 {
		t.Fatalf("after retention: %+v", pts)
	}
}

func TestProfilesSkipGapsAndAverageReplicas(t *testing.T) {
	s := open(t)
	t0 := time.Now().Add(-3 * time.Hour).Truncate(time.Hour)
	// 10 minutes, then a 60-minute gap (app closed), then 10 more.
	for _, start := range []time.Time{t0, t0.Add(70 * time.Minute)} {
		for i := range 10 {
			if err := s.Add(start.Add(time.Duration(i)*time.Minute), map[Series]Sample{web1: {CPU: 20, Mem: 100 << 20}, web2: {CPU: 60, Mem: 300 << 20}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	ps, err := s.Profiles("c", "pod", t0.Add(-time.Hour))
	if err != nil || len(ps) != 1 {
		t.Fatalf("profiles: %+v %v", ps, err)
	}
	p := ps[0]
	// Per replica: (20+60)/2 = 40m typical, max reading 60m; 20 minutes recorded over an 80-minute span.
	if p.Minutes != 20 || p.CPUMed != 40 || p.CPUMax != 60 || p.MemMed != 200<<20 || p.MemMax != 300<<20 || p.Last-p.First != 80*60 {
		t.Fatalf("profile: %+v", p)
	}
}

func TestOOMsDedupeAndPrune(t *testing.T) {
	s := open(t)
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	old := time.Now().Add(-30 * 24 * time.Hour)
	ev := []OOM{{"c", "ns", "Deployment/web", "app", "web-1", at}, {"c", "ns", "Deployment/web", "app", "web-1", at}, // seen twice
		{"c", "ns", "Deployment/web", "app", "web-2", at.Add(time.Minute)}, {"c", "ns", "Deployment/web", "app", "web-3", old}}
	if err := s.AddOOMs(ev); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(14 * 24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	m, err := s.OOMs("c", time.Now().Add(-365*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if st := m["ns/Deployment/web/app"]; st.Count != 2 || !st.Last.Equal(at.Add(time.Minute)) {
		t.Fatalf("ooms: %+v", m)
	}
}

func TestDecodeBlockRejectsDamage(t *testing.T) {
	good := encodeBlock(tier5, block{0: {N: 5, CPU: 120, CPUMin: 100, CPUMax: 150, Mem: 2048, MemMin: 2000, MemMax: 2100}})
	cases := map[string]struct {
		tier int
		data []byte
	}{
		"unknown tier 0":   {0, good},
		"negative tier":    {-5, good},
		"tier 7":           {7, good},
		"empty":            {tier5, nil},
		"bitmap only":      {tier5, good[:2]},
		"truncated varint": {tier5, append(append([]byte{}, good[:2]...), 0x80)},
		"overlong varint":  {tier1, append([]byte{1, 0, 0, 0, 0, 0, 0, 0}, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01)},
	}
	for name, c := range cases {
		if _, err := decodeBlock(c.tier, c.data); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if b, err := decodeBlock(tier5, good); err != nil || b[0].CPU != 120 {
		t.Errorf("round trip: %v %v", b, err)
	}
}
