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
