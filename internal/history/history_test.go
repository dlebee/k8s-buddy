package history

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreBucketsAndQueries(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t0 := time.Now().Add(-time.Hour).Truncate(Bucket)
	a := Series{Cluster: "c", Kind: "pod", Namespace: "ns", Name: "web-1", Owner: "Deployment/web", CPUReq: 100, MemReq: 1 << 20, CPULim: -1, MemLim: -1}
	b := Series{Cluster: "c", Kind: "pod", Namespace: "ns", Name: "web-2", Owner: "Deployment/web", CPUReq: 100, MemReq: 1 << 20, CPULim: -1, MemLim: -1}
	// Two samples in the first bucket, one in the next.
	if err := s.Add(t0.Add(time.Minute), map[Series]Sample{a: {CPU: 10, Mem: 100}, b: {CPU: 30, Mem: 300}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(t0.Add(2*time.Minute), map[Series]Sample{a: {CPU: 50, Mem: 100}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(t0.Add(Bucket+time.Minute), map[Series]Sample{a: {CPU: 20, Mem: 200}}); err != nil {
		t.Fatal(err)
	}
	pts, info, err := s.Query("c", "pod", "ns", "web-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 || pts[0].CPU != 30 || pts[0].CPUMax != 50 || pts[1].CPU != 20 || info.CPUReq != 100 {
		t.Fatalf("pod: %+v %+v", pts, info)
	}
	w, wi, err := s.Query("c", "workload", "ns", "Deployment/web", t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(w) != 2 || w[0].CPU != 60 || w[0].Mem != 400 || wi.Pods != 2 || wi.CPUReq != 200 {
		t.Fatalf("workload: %+v %+v", w, wi)
	}
	if err := s.Prune(30 * time.Minute); err != nil {
		t.Fatal(err)
	}
	if pts, _, _ := s.Query("c", "pod", "ns", "web-1", t0); len(pts) != 0 {
		t.Fatalf("prune kept %d points", len(pts))
	}
}

func TestProfilesSkipGapsAndAverageReplicas(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t0 := time.Now().Add(-48 * time.Hour).Truncate(Bucket)
	a := Series{Cluster: "c", Kind: "pod", Namespace: "ns", Name: "web-1", Owner: "Deployment/web", CPUReq: -1, MemReq: -1, CPULim: -1, MemLim: -1}
	b := Series{Cluster: "c", Kind: "pod", Namespace: "ns", Name: "web-2", Owner: "Deployment/web", CPUReq: -1, MemReq: -1, CPULim: -1, MemLim: -1}
	// Three buckets, with a 24-hour gap (app closed) between the second and third.
	for i, at := range []time.Time{t0, t0.Add(Bucket), t0.Add(24 * time.Hour)} {
		if err := s.Add(at.Add(time.Second), map[Series]Sample{a: {CPU: int64(10 * (i + 1)), Mem: 100}, b: {CPU: int64(30 * (i + 1)), Mem: 300}}); err != nil {
			t.Fatal(err)
		}
	}
	ps, err := s.Profiles("c", "pod", t0.Add(-time.Hour))
	if err != nil || len(ps) != 1 {
		t.Fatalf("profiles: %+v %v", ps, err)
	}
	p := ps[0]
	// Per-replica averages per bucket: 20, 40, 60 → median 40. Max reading 90. Memory per replica 200, max 300.
	if p.Buckets != 3 || p.CPUMed != 40 || p.CPUMax != 90 || p.MemMed != 200 || p.MemMax != 300 || p.Last-p.First != int64(24*time.Hour/time.Second) {
		t.Fatalf("profile: %+v", p)
	}
}

func TestOOMsDedupeAndPrune(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
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
