package monitor

import (
	"testing"
	"time"

	"github.com/dlebee/k8s-buddy/internal/alert"
)

func TestMinorOf(t *testing.T) {
	cases := map[string]string{"v1.28.5": "1.28", "v1.31.2-gke.1000": "1.31", "1.30.0+k3s1": "1.30", "": "", "garbage": ""}
	for in, want := range cases {
		if got := minorOf(in); got != want {
			t.Errorf("minorOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEOLSeverity(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		eol  time.Time
		want alert.Severity
		ok   bool
	}{
		{now.AddDate(0, 3, 0), "", false},             // 3 months out: nothing
		{now.AddDate(0, 2, -1), alert.Warning, true},  // just inside 2 months
		{now.AddDate(0, 1, -1), alert.Critical, true}, // inside 1 month
		{now.AddDate(0, 0, -5), alert.Critical, true}, // past
	} {
		sev, title, ok := eolSeverity(eolEntry{Minor: "1.35", EOL: tc.eol}, now)
		if ok != tc.ok || sev != tc.want {
			t.Errorf("eol %s: got %q %v (%s), want %q %v", tc.eol.Format("2006-01-02"), sev, ok, title, tc.want, tc.ok)
		}
	}
}
