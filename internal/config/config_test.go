package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizeTags(t *testing.T) {
	got := normalizeTags(map[string][]string{"a": {" Prod ", "prod", "East US", "", "x<y>"}, "b": {"  "}})
	if len(got) != 1 || len(got["a"]) != 3 || got["a"][0] != "east-us" || got["a"][1] != "prod" || got["a"][2] != "x-y" {
		t.Fatalf("got %v", got)
	}
}

func TestBetaFeaturesNormalized(t *testing.T) {
	s := Settings{Beta: []string{"cost", "nope", "cost"}}
	s.normalize()
	if len(s.Beta) != 1 || !s.BetaOn("cost") || s.BetaOn("nope") {
		t.Fatalf("beta: %v", s.Beta)
	}
	if d := Defaults(); d.BetaOn("cost") {
		t.Fatal("cost must be off by default")
	}
}

func TestRetentionMovesToFourteenDays(t *testing.T) {
	old := Settings{UsageRetentionDays: 7, UsageHistorySet: true}
	old.normalize()
	if old.UsageRetentionDays != 14 || !old.UsageRetention14 {
		t.Fatalf("old default: %+v", old.UsageRetentionDays)
	}
	chose := Settings{UsageRetentionDays: 7, UsageHistorySet: true, UsageRetention14: true}
	chose.normalize()
	if chose.UsageRetentionDays != 7 {
		t.Fatalf("explicit 7 days changed to %d", chose.UsageRetentionDays)
	}
	custom := Settings{UsageRetentionDays: 30, UsageHistorySet: true}
	custom.normalize()
	if custom.UsageRetentionDays != 30 {
		t.Fatalf("custom retention changed to %d", custom.UsageRetentionDays)
	}
}

func TestOpenMigratesOldRetention(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"usageRetentionDays": 7, "usageHistory": true, "usageHistorySet": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if d := st.Get().UsageRetentionDays; d != 14 {
		t.Fatalf("old 7-day default not moved: %d", d)
	}
	// Saved with the flag, an explicit 7 stays.
	if err := st.Update(func(s *Settings) { s.UsageRetentionDays = 7 }); err != nil {
		t.Fatal(err)
	}
	st2, _ := Open(dir)
	if d := st2.Get().UsageRetentionDays; d != 7 {
		t.Fatalf("explicit 7 days not kept: %d", d)
	}
}

func TestDoctorEvery(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 6 * time.Hour, "6h": 6 * time.Hour, "3h": 3 * time.Hour, "24h": 24 * time.Hour, "off": 0, "5m": 6 * time.Hour} {
		s := Settings{DoctorEvery: in}
		s.normalize()
		if got := s.DoctorInterval(); got != want {
			t.Errorf("DoctorEvery %q: %v, want %v", in, got, want)
		}
	}
}
