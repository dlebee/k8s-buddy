package monitor

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dlebee/k8s-buddy/internal/alert"
	"github.com/dlebee/k8s-buddy/internal/config"
)

func TestDoctorSchedule(t *testing.T) {
	st, err := config.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(st, alert.NewStore(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Before any checkup: a few minutes after launch.
	if got := m.nextDoctor(DoctorReport{}); !got.Equal(m.started.Add(doctorWarmup)) {
		t.Fatalf("first checkup at %v", got)
	}
	// After one (by the user or the schedule): 6 hours later by default.
	done := time.Now().Add(-time.Hour)
	if got := m.nextDoctor(DoctorReport{FinishedAt: done}); !got.Equal(done.Add(6 * time.Hour)) {
		t.Fatalf("next checkup at %v", got)
	}
	// One long overdue (saved before a restart): still waits for the clusters to load.
	old := time.Now().Add(-2 * 24 * time.Hour)
	if got := m.nextDoctor(DoctorReport{FinishedAt: old}); !got.Equal(m.started.Add(doctorWarmup)) {
		t.Fatalf("overdue checkup at %v", got)
	}
	// Turned off: no next checkup.
	if err := st.Update(func(s *config.Settings) { s.DoctorEvery = "off" }); err != nil {
		t.Fatal(err)
	}
	if got := m.nextDoctor(DoctorReport{FinishedAt: done}); !got.IsZero() {
		t.Fatalf("schedule off, next at %v", got)
	}

	// While a new checkup runs, the buddy keeps the previous result.
	m.doc.done = DoctorReport{FinishedAt: done, Findings: []Finding{{Cluster: "a", Name: "web"}}}
	m.doc.report = DoctorReport{Running: true}
	if r := m.LastDoctor(); !r.Running || len(r.Findings) != 1 || !r.FinishedAt.Equal(done) {
		t.Fatalf("last doctor while running: %+v", r)
	}
}

// The latest checkup survives a restart; a file from another format, a stale one, and findings
// for clusters excluded since are not brought back.
func TestDoctorSavedAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	st, err := config.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	file := filepath.Join(dir, "doctor.json")
	restart := func() *Manager {
		m := NewManager(st, alert.NewStore(), log)
		m.doc.file = file
		m.loadDoctor()
		return m
	}
	m := restart()
	done := time.Now().Add(-time.Hour).Truncate(time.Second)
	m.saveDoctor(DoctorReport{FinishedAt: done, Clusters: []string{"a", "b"},
		Findings: []Finding{{Cluster: "a", Name: "web"}, {Cluster: "b", Name: "api"}}})

	r := restart().LastDoctor()
	if !r.FinishedAt.Equal(done) || len(r.Findings) != 2 || r.Running {
		t.Fatalf("after restart: %+v", r)
	}
	// Only the latest is kept: the next save replaces it.
	restart().saveDoctor(DoctorReport{FinishedAt: done.Add(time.Minute), Findings: []Finding{{Cluster: "a", Name: "db"}}})
	if r := restart().LastDoctor(); len(r.Findings) != 1 || r.Findings[0].Name != "db" {
		t.Fatalf("replaced: %+v", r.Findings)
	}
	// Cluster b excluded since: its findings aren't shown.
	restart().saveDoctor(DoctorReport{FinishedAt: done, Clusters: []string{"a", "b"},
		Findings: []Finding{{Cluster: "a", Name: "web"}, {Cluster: "b", Name: "api"}}})
	if err := st.Update(func(s *config.Settings) { s.ExcludedContexts = []string{"b"} }); err != nil {
		t.Fatal(err)
	}
	if r := restart().LastDoctor(); len(r.Findings) != 1 || r.Findings[0].Cluster != "a" || len(r.Clusters) != 1 {
		t.Fatalf("excluded: %+v", r)
	}
	// Another format version, or too old: ignored.
	for name, body := range map[string]string{
		"version": `{"version":99,"report":{"finishedAt":"` + done.Format(time.RFC3339) + `","findings":[{"cluster":"a","name":"x"}]}}`,
		"stale":   `{"version":1,"report":{"finishedAt":"` + done.Add(-30*24*time.Hour).Format(time.RFC3339) + `","findings":[{"cluster":"a","name":"x"}]}}`,
		"garbage": `not json`,
	} {
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if r := restart().LastDoctor(); !r.FinishedAt.IsZero() || len(r.Findings) != 0 {
			t.Fatalf("%s: %+v", name, r)
		}
	}
}
