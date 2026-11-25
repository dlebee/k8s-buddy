package monitor

import (
	"slices"
	"strings"
	"time"

	"github.com/dlebee/k8s-buddy/internal/history"
)

// Spikes lists workloads and nodes whose CPU or memory jumped well above their usual use. It's for
// looking, not for sizing: it never changes a Quotas verdict or suggestion.

// spikeRules: CPU at least 2x usual and 200m more (500m for a node); memory at least 1.5x usual
// and 256Mi more (1Gi for a node).
var spikeRules = map[string][2]history.SpikeRule{
	"pod":  {{Ratio: 2, Floor: 200}, {Ratio: 1.5, Floor: 256 << 20}},
	"node": {{Ratio: 2, Floor: 500}, {Ratio: 1.5, Floor: 1 << 30}},
}

// SpikeRow is one workload (replicas together) or node, for one resource.
type SpikeRow struct {
	Cluster   string  `json:"cluster"`
	Kind      string  `json:"kind"` // "Node", or the workload kind ("Deployment", …; "Pod" when it has no owner)
	Namespace string  `json:"namespace,omitempty"`
	Name      string  `json:"name"`
	Resource  string  `json:"resource"` // "cpu" or "memory"
	Usual     float64 `json:"usual"`    // millicores or bytes: the usual use of the pod or node that spiked highest
	Peak      float64 `json:"peak"`
	PeakAt    int64   `json:"peakAt"`
	LastAt    int64   `json:"lastAt"`  // start of the latest spike
	Spikes    int     `json:"spikes"`  // spikes across its pods
	Minutes   int64   `json:"minutes"` // how long they lasted in total
	Pods      int     `json:"pods"`    // pods that spiked (workloads)
	Limit     int64   `json:"limit"`   // per pod limit, or the node's allocatable; -1: none
	System    bool    `json:"system"`
	// For the chart: what to ask /api/usage for.
	HistoryKind string `json:"historyKind"`
	HistoryName string `json:"historyName"`
}

type SpikeReport struct {
	Available     bool       `json:"available"`
	Days          int        `json:"days"`
	RetentionDays int        `json:"retentionDays"`
	Clusters      []string   `json:"clusters"`
	Rows          []SpikeRow `json:"rows"`
}

// Spikes looks back `days` days (capped at the history kept) in the given clusters (all when empty).
func (m *Manager) Spikes(clusters []string, days int) SpikeReport {
	s := m.settings.Get()
	days = min(max(days, 1), max(s.UsageRetentionDays, 1))
	rep := SpikeReport{Days: days, RetentionDays: s.UsageRetentionDays, Clusters: []string{}, Rows: []SpikeRow{}}
	h := m.History()
	if h == nil || !s.UsageHistory {
		return rep
	}
	rep.Available = true
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	for _, c := range m.connected() {
		if len(clusters) > 0 && !slices.Contains(clusters, c.name) {
			continue
		}
		rep.Clusters = append(rep.Clusters, c.name)
		series, err := h.Spikes(c.name, since, spikeRules)
		if err != nil {
			continue
		}
		rep.Rows = append(rep.Rows, spikeRows(c.name, series)...)
	}
	slices.SortFunc(rep.Rows, func(a, b SpikeRow) int {
		if a.LastAt != b.LastAt {
			return int(b.LastAt - a.LastAt)
		}
		return strings.Compare(a.Cluster+a.Namespace+a.Name+a.Resource, b.Cluster+b.Namespace+b.Name+b.Resource)
	})
	return rep
}

// spikeRows folds pods into their workload, per resource; nodes stay one row each.
func spikeRows(cluster string, series []history.SeriesSpikes) []SpikeRow {
	rows := map[string]*SpikeRow{}
	add := func(sr history.SeriesSpikes, resource string, evs []history.SpikeEvent, usual float64, limit int64) {
		if len(evs) == 0 {
			return
		}
		r := SpikeRow{Cluster: cluster, Resource: resource, Limit: limit}
		if sr.Kind == "node" {
			r.Kind, r.Name, r.HistoryKind, r.HistoryName = "Node", sr.Name, "node", sr.Name
		} else {
			r.Namespace, r.System = sr.Namespace, systemNamespace(sr.Namespace)
			r.Kind, r.Name, r.HistoryKind, r.HistoryName = "Pod", sr.Name, "pod", sr.Name
			if kind, name, ok := strings.Cut(sr.Owner, "/"); ok {
				r.Kind, r.Name, r.HistoryKind, r.HistoryName = kind, name, "workload", sr.Owner
			}
		}
		key := r.Kind + "/" + r.Namespace + "/" + r.Name + "/" + resource
		cur := rows[key]
		if cur == nil {
			cur = &r
			rows[key] = cur
		}
		cur.Pods++
		for _, e := range evs {
			cur.Spikes++
			cur.Minutes += (e.End - e.Start) / 60
			cur.LastAt = max(cur.LastAt, e.Start)
			if e.Peak > cur.Peak {
				cur.Peak, cur.PeakAt, cur.Usual, cur.Limit = e.Peak, e.PeakAt, usual, limit
			}
		}
	}
	for _, sr := range series {
		add(sr, "cpu", sr.CPU, sr.CPUUsual, sr.CPULim)
		add(sr, "memory", sr.Mem, sr.MemUsual, sr.MemLim)
	}
	out := make([]SpikeRow, 0, len(rows))
	for _, r := range rows {
		if r.Kind == "Node" {
			r.Pods = 0
		}
		out = append(out, *r)
	}
	return out
}
