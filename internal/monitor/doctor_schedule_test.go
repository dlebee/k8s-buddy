package monitor

import (
	"io"
	"log/slog"
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
	done := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	if got := m.nextDoctor(DoctorReport{FinishedAt: done}); !got.Equal(done.Add(6 * time.Hour)) {
		t.Fatalf("next checkup at %v", got)
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
