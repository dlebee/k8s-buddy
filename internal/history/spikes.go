package history

import "time"

// SpikeRule says when a reading counts as a spike: above Pct percent of the series' limit (a
// pod's summed container limits, a node's allocatable). Series without a limit aren't judged.
type SpikeRule struct {
	Pct float64
}

// SpikeEvent is one run of consecutive points above the rule.
type SpikeEvent struct {
	Start, End int64   // unix seconds
	Peak       float64 // highest reading in it
	PeakAt     int64   // start of the point that held the peak
}

// SpikeSkips counts series that couldn't be judged because they have no limit.
type SpikeSkips struct {
	PodCPU, PodMem int
}

// SeriesSpikes are the spikes of one pod or node in a window.
type SeriesSpikes struct {
	Kind, Namespace, Name, Owner string
	CPULim, MemLim               int64 // latest recorded: pods' summed limits, nodes' allocatable (-1: none)
	CPUUsual, MemUsual           float64
	CPU, Mem                     []SpikeEvent
}

// Spikes finds where pods and nodes of a cluster went above a share of their limit since `since`
// (rules per kind: CPU, memory). A point is a spike when its highest reading passes the rule. Usual
// is the median reading weighted by minutes, for context. Time k8s-buddy wasn't recording is
// skipped. Only series with at least one spike are returned.
func (s *Store) Spikes(cluster string, since time.Time, rules map[string][2]SpikeRule) ([]SeriesSpikes, SpikeSkips, error) {
	var skips SpikeSkips
	byID, _, err := s.readPoints(since.Unix(), `se.cluster = ? AND se.kind IN ('pod', 'node')`, cluster)
	if err != nil {
		return nil, skips, err
	}
	meta := map[int64]SeriesSpikes{}
	rows, err := s.db.Query(`SELECT id, kind, ns, name, owner, cpu_lim, mem_lim FROM series WHERE cluster = ? AND kind IN ('pod', 'node')`, cluster)
	if err != nil {
		return nil, skips, err
	}
	for rows.Next() {
		var id int64
		var m SeriesSpikes
		if rows.Scan(&id, &m.Kind, &m.Namespace, &m.Name, &m.Owner, &m.CPULim, &m.MemLim) == nil {
			meta[id] = m
		}
	}
	rows.Close()

	var out []SeriesSpikes
	for id, pts := range byID {
		m, ok := meta[id]
		rule, ok2 := rules[m.Kind]
		if !ok || !ok2 || len(pts) == 0 {
			continue
		}
		cpu, mem, w := make([]float64, len(pts)), make([]float64, len(pts)), make([]float64, len(pts))
		for i, p := range pts {
			cpu[i], mem[i], w[i] = float64(p.b.CPU), float64(p.b.Mem*1024), float64(p.b.N)
		}
		m.CPUUsual, m.MemUsual = wpct(cpu, w, .5), wpct(mem, w, .5)
		if m.Kind == "pod" && m.CPULim <= 0 {
			skips.PodCPU++
		}
		if m.Kind == "pod" && m.MemLim <= 0 {
			skips.PodMem++
		}
		m.CPU = events(pts, func(p tierPoint) float64 { return float64(p.b.CPUMax) }, float64(m.CPULim), rule[0])
		m.Mem = events(pts, func(p tierPoint) float64 { return float64(p.b.MemMax * 1024) }, float64(m.MemLim), rule[1])
		if len(m.CPU)+len(m.Mem) > 0 {
			out = append(out, m)
		}
	}
	return out, skips, nil
}

// events joins consecutive points (sorted by time) above Pct of limit into spikes.
func events(pts []tierPoint, peak func(tierPoint) float64, limit float64, rule SpikeRule) []SpikeEvent {
	if limit <= 0 || rule.Pct <= 0 {
		return nil
	}
	over := limit * rule.Pct / 100
	var out []SpikeEvent
	for _, p := range pts {
		v := peak(p)
		if v <= over {
			continue
		}
		if n := len(out); n > 0 && p.t <= out[n-1].End {
			e := &out[n-1]
			e.End = p.t + p.step
			if v > e.Peak {
				e.Peak, e.PeakAt = v, p.t
			}
			continue
		}
		out = append(out, SpikeEvent{Start: p.t, End: p.t + p.step, Peak: v, PeakAt: p.t})
	}
	return out
}
