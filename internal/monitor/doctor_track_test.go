package monitor

import (
	"testing"
	"time"
)

func TestTrackFindings(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(6 * time.Hour)
	a := Finding{Cluster: "a", Check: "unused-pvc", Kind: "PersistentVolumeClaim", Namespace: "shop", Name: "data"}
	b := Finding{Cluster: "a", Check: "naked-pods", Kind: "Pod", Namespace: "shop", Name: "web"}
	other := Finding{Cluster: "b", Check: "naked-pods", Kind: "Pod", Namespace: "api", Name: "job"}

	// First checkup: everything is new.
	found, resolved := trackFindings(DoctorReport{}, []Finding{a, b, other}, []string{"a", "b"}, t0)
	if !found[0].FirstSeen.Equal(t0) || len(resolved) != 0 {
		t.Fatalf("first run: %+v %+v", found, resolved)
	}
	prev := DoctorReport{StartedAt: t0, Findings: found}

	// Second: a stays (message changed), b is fixed, cluster b wasn't checked so other isn't resolved.
	a2 := a
	a2.Message = "changed"
	found, resolved = trackFindings(prev, []Finding{a2}, []string{"a"}, t1)
	if !found[0].FirstSeen.Equal(t0) {
		t.Fatalf("carried first seen: %v", found[0].FirstSeen)
	}
	if len(resolved) != 1 || resolved[0].Name != "web" || !resolved[0].ResolvedAt.Equal(t1) || !resolved[0].FirstSeen.Equal(t0) {
		t.Fatalf("resolved: %+v", resolved)
	}
	prev = DoctorReport{StartedAt: t1, Findings: found, Resolved: resolved}

	// b comes back: it leaves Resolved and is new again.
	t2 := t1.Add(time.Hour)
	found, resolved = trackFindings(prev, []Finding{a, b}, []string{"a"}, t2)
	if len(resolved) != 0 || !found[1].FirstSeen.Equal(t2) || !found[0].FirstSeen.Equal(t0) {
		t.Fatalf("came back: %+v %+v", found, resolved)
	}

	// Resolved entries expire.
	old := DoctorReport{Resolved: []Finding{{Name: "x", ResolvedAt: t0}}}
	if _, r := trackFindings(old, nil, nil, t0.Add(resolvedKeep+time.Minute)); len(r) != 0 {
		t.Fatalf("expired kept: %+v", r)
	}

	// A finding saved without FirstSeen (older version) counts from that checkup's start.
	legacy := DoctorReport{StartedAt: t0, Findings: []Finding{a}}
	if f, _ := trackFindings(legacy, []Finding{a}, []string{"a"}, t1); !f[0].FirstSeen.Equal(t0) {
		t.Fatalf("legacy: %v", f[0].FirstSeen)
	}
}
