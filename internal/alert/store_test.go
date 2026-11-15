package alert

import (
	"testing"
	"time"
)

func TestExcludedClusterAlertsClearAndStayCleared(t *testing.T) {
	s := NewStore()
	s.Observe(Alert{Key: "unreachable", Source: "connectivity", Cluster: "stale", Severity: Notice, Title: "Can't reach stale"})
	s.Observe(Alert{Key: "pvc-pending/a/b", Source: "pvcs", Cluster: "live", Severity: Warning, Title: "PVC stuck"})

	s.Configure([]Filter{ExcludeClusters([]string{"stale"})}, 100, time.Hour, Notice)
	if got := s.ActiveByCluster(); got["stale"] != 0 || got["live"] != 1 {
		t.Fatalf("after exclude: %v", got)
	}

	// A monitor that was mid-request when it got cancelled reports late.
	s.Observe(Alert{Key: "unreachable", Source: "connectivity", Cluster: "stale", Severity: Notice, Title: "Can't reach stale"})
	if got := s.ActiveByCluster(); got["stale"] != 0 {
		t.Fatalf("late report re-raised alert: %v", got)
	}

	s.PurgeClusters([]string{"stale"})
	for _, a := range s.List(Query{Scope: "all"}) {
		if a.Cluster == "stale" {
			t.Fatalf("excluded cluster still in history: %+v", a)
		}
	}
	if n := len(s.List(Query{Scope: "all"})); n != 1 {
		t.Fatalf("history len = %d, want 1 (live cluster only)", n)
	}
}

func TestStateAlertResolvesWhenMissing(t *testing.T) {
	s := NewStore()
	s.Observe(Alert{Key: "k1", Source: "pods", Cluster: "c", Severity: Warning})
	s.Observe(Alert{Key: "k2", Source: "pods", Cluster: "c", Severity: Warning})
	s.ResolveMissing("c", "pods", map[string]bool{"k2": true})
	if sum := s.Summary(); sum.Pending != 1 {
		t.Fatalf("pending = %d, want 1", sum.Pending)
	}
	if h := s.List(Query{Scope: "all"}); len(h) != 2 || !h[1].Resolved {
		t.Fatalf("history should keep the resolved alert: %+v", h)
	}
}

func TestTransientCountsAndSnooze(t *testing.T) {
	s := NewStore()
	now := time.Now()
	s.now = func() time.Time { return now }
	s.Record(Alert{Key: "restart/x", Cluster: "c", Severity: Warning, Count: 2})
	s.Record(Alert{Key: "restart/x", Cluster: "c", Severity: Warning})
	l := s.List(Query{})
	if len(l) != 1 || l[0].Count != 3 {
		t.Fatalf("got %+v", l)
	}
	s.Ack(l[0].ID)
	if s.Summary().Pending != 0 {
		t.Fatal("snoozed alert still pending")
	}
	now = now.Add(5 * time.Hour)
	if s.Summary().Pending != 1 {
		t.Fatal("snooze should expire")
	}
}

func TestRetainClustersDropsUnknown(t *testing.T) {
	s := NewStore()
	s.Observe(Alert{Key: "a", Cluster: "gone", Severity: Warning})
	s.Observe(Alert{Key: "b", Cluster: "kept", Severity: Warning})
	s.RetainClusters([]string{"kept"})
	h := s.List(Query{Scope: "all"})
	if len(h) != 1 || h[0].Cluster != "kept" {
		t.Fatalf("got %+v", h)
	}
}

func TestListAndAckAllByClusters(t *testing.T) {
	s := NewStore()
	for _, c := range []string{"a", "b", "c"} {
		s.Observe(Alert{Key: "k", Cluster: c, Severity: Warning})
	}
	if n := len(s.List(Query{Clusters: []string{"a", "c"}})); n != 2 {
		t.Fatalf("list by clusters: %d", n)
	}
	s.AckAll([]string{"a", "b"})
	if l := s.List(Query{}); len(l) != 1 || l[0].Cluster != "c" {
		t.Fatalf("ack-all by clusters: %+v", l)
	}
}
