package alert

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

type Severity string

const (
	Info     Severity = "info"
	Notice   Severity = "notice"
	Warning  Severity = "warning"
	Critical Severity = "critical"
)

func (s Severity) Rank() int {
	switch s {
	case Critical:
		return 3
	case Warning:
		return 2
	case Notice:
		return 1
	default:
		return 0
	}
}

// Alert is one deduplicated problem. State alerts mirror a condition that is true right now
// (resolved by the detector when it clears); transient alerts record occurrences (restarts,
// events) and expire after a quiet period.
type Alert struct {
	ID         string    `json:"id"`
	Key        string    `json:"key"`
	Source     string    `json:"source"`
	Cluster    string    `json:"cluster"`
	Severity   Severity  `json:"severity"`
	Category   string    `json:"category"`
	Kind       string    `json:"kind,omitempty"`
	Namespace  string    `json:"namespace,omitempty"`
	Name       string    `json:"name,omitempty"`
	Title      string    `json:"title"`
	Detail     string    `json:"detail,omitempty"`
	Hint       string    `json:"hint,omitempty"`
	Transient  bool      `json:"transient"`
	Count      int       `json:"count"`
	FirstSeen  time.Time `json:"firstSeen"`
	LastSeen   time.Time `json:"lastSeen"`
	Resolved   bool      `json:"resolved"`
	ResolvedAt time.Time `json:"resolvedAt,omitzero"`
	AckedAt    time.Time `json:"ackedAt,omitzero"`
}

// FullKey makes keys unique across clusters.
func (a *Alert) FullKey() string { return a.Cluster + "|" + a.Key }

// Pending reports whether the alert still wants the user's attention.
func (a *Alert) Pending(now time.Time, snooze time.Duration) bool {
	if a.Resolved {
		return false
	}
	return a.AckedAt.IsZero() || now.After(a.AckedAt.Add(snooze))
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
