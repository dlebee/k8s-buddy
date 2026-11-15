package alert

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"
	"sync"
	"time"
)

type EventType string

const (
	EventNew      EventType = "new"
	EventUpdate   EventType = "update"
	EventResolve  EventType = "resolve"
	EventChanged  EventType = "changed"
	transientLife           = 30 * time.Minute
)

type Event struct {
	Type  EventType `json:"type"`
	Alert *Alert    `json:"alert,omitempty"`
}

type Summary struct {
	Pending   int              `json:"pending"`
	Nudgeable int              `json:"nudgeable"`
	Active    int              `json:"active"`
	History   int              `json:"history"`
	BySev     map[Severity]int `json:"bySeverity"`
	Clusters  int              `json:"clusters"`
}

type Store struct {
	mu          sync.Mutex
	active      map[string]*Alert
	history     []*Alert
	filters     []Filter
	historySize int
	snooze      time.Duration
	nudgeLevel  Severity
	subs        map[chan Event]struct{}
	dirty       bool
	now         func() time.Time
}

func NewStore() *Store {
	return &Store{
		active:      map[string]*Alert{},
		historySize: 1000,
		snooze:      4 * time.Hour,
		nudgeLevel:  Notice,
		subs:        map[chan Event]struct{}{},
		now:         time.Now,
	}
}

func (s *Store) Configure(filters []Filter, historySize int, snooze time.Duration, nudge Severity) {
	s.mu.Lock()
	s.filters = filters
	s.historySize = historySize
	s.snooze = snooze
	s.nudgeLevel = nudge
	var dropped []*Alert
	for k, a := range s.active {
		if !s.allowLocked(*a) {
			s.resolveLocked(a)
			delete(s.active, k)
			dropped = append(dropped, a)
		}
	}
	s.pruneLocked()
	s.mu.Unlock()
	for _, a := range dropped {
		s.publish(Event{Type: EventResolve, Alert: a})
	}
	s.publish(Event{Type: EventChanged})
}

func (s *Store) allowLocked(a Alert) bool {
	for _, f := range s.filters {
		if !f.Allow(context.Background(), a) {
			return false
		}
	}
	return true
}

// Observe asserts a state alert is currently true.
func (s *Store) Observe(a Alert) {
	a.Transient = false
	s.upsert(a, false)
}

// Record logs an occurrence of a transient alert; a.Count is the number of occurrences (min 1).
func (s *Store) Record(a Alert) {
	a.Transient = true
	s.upsert(a, true)
}

func (s *Store) upsert(a Alert, add bool) {
	s.mu.Lock()
	if !s.allowLocked(a) {
		s.mu.Unlock()
		return
	}
	now := s.now()
	if a.Count < 1 {
		a.Count = 1
	}
	fk := a.FullKey()
	var ev Event
	if cur, ok := s.active[fk]; ok {
		changed := add || cur.Title != a.Title || cur.Detail != a.Detail || cur.Severity != a.Severity
		if add {
			cur.Count += a.Count
		} else {
			cur.Count = a.Count
		}
		cur.Title, cur.Detail, cur.Hint, cur.Severity = a.Title, a.Detail, a.Hint, a.Severity
		cur.LastSeen = now
		if !changed {
			s.mu.Unlock()
			return
		}
		s.dirty = true
		cp := *cur
		ev = Event{Type: EventUpdate, Alert: &cp}
	} else {
		a.ID = newID()
		a.FirstSeen, a.LastSeen = now, now
		na := a
		s.active[fk] = &na
		s.history = append(s.history, &na)
		s.pruneLocked()
		s.dirty = true
		cp := na
		ev = Event{Type: EventNew, Alert: &cp}
	}
	s.mu.Unlock()
	s.publish(ev)
}

// ResolveMissing resolves state alerts from source on cluster whose keys are not in present.
func (s *Store) ResolveMissing(cluster, source string, present map[string]bool) {
	s.resolveWhere(func(a *Alert) bool {
		return !a.Transient && a.Cluster == cluster && a.Source == source && !present[a.Key]
	})
}

// ResolveCluster resolves everything for a cluster (e.g. it was excluded or removed).
func (s *Store) ResolveCluster(cluster string) {
	s.resolveWhere(func(a *Alert) bool { return a.Cluster == cluster })
}

// ExpireTransient resolves transient alerts that have been quiet for a while.
func (s *Store) ExpireTransient() {
	cutoff := s.now().Add(-transientLife)
	s.resolveWhere(func(a *Alert) bool { return a.Transient && a.LastSeen.Before(cutoff) })
}

func (s *Store) resolveWhere(match func(*Alert) bool) {
	s.mu.Lock()
	var resolved []*Alert
	for k, a := range s.active {
		if match(a) {
			s.resolveLocked(a)
			delete(s.active, k)
			cp := *a
			resolved = append(resolved, &cp)
		}
	}
	if len(resolved) > 0 {
		s.dirty = true
		s.pruneLocked()
	}
	s.mu.Unlock()
	for _, a := range resolved {
		s.publish(Event{Type: EventResolve, Alert: a})
	}
}

func (s *Store) resolveLocked(a *Alert) {
	a.Resolved = true
	a.ResolvedAt = s.now()
}

func (s *Store) pruneLocked() {
	over := len(s.history) - s.historySize
	if over <= 0 {
		return
	}
	kept := s.history[:0]
	for _, a := range s.history {
		if over > 0 && a.Resolved {
			over--
			continue
		}
		kept = append(kept, a)
	}
	clear(s.history[len(kept):])
	s.history = kept
}

// RetainClusters drops every alert (history included) of clusters not in names.
func (s *Store) RetainClusters(names []string) {
	keep := make(map[string]bool, len(names))
	for _, n := range names {
		keep[n] = true
	}
	s.mu.Lock()
	var drop []string
	seen := map[string]bool{}
	for _, a := range s.history {
		if !keep[a.Cluster] && !seen[a.Cluster] {
			seen[a.Cluster] = true
			drop = append(drop, a.Cluster)
		}
	}
	s.mu.Unlock()
	s.PurgeClusters(drop)
}

// PurgeClusters drops every alert of the given clusters, history included.
func (s *Store) PurgeClusters(names []string) {
	if len(names) == 0 {
		return
	}
	drop := make(map[string]bool, len(names))
	for _, n := range names {
		drop[n] = true
	}
	s.mu.Lock()
	n := len(s.history)
	s.history = slices.DeleteFunc(s.history, func(a *Alert) bool { return drop[a.Cluster] })
	for k, a := range s.active {
		if drop[a.Cluster] {
			delete(s.active, k)
		}
	}
	changed := len(s.history) != n
	if changed {
		s.dirty = true
	}
	s.mu.Unlock()
	if changed {
		s.publish(Event{Type: EventChanged})
	}
}

// Touch tells subscribers something outside the alert list changed (e.g. a sign-in finished).
func (s *Store) Touch() { s.publish(Event{Type: EventChanged}) }

func (s *Store) Ack(id string) bool {
	return s.ackWhere(func(a *Alert) bool { return a.ID == id })
}

func (s *Store) AckAll(cluster string) bool {
	return s.ackWhere(func(a *Alert) bool { return cluster == "" || a.Cluster == cluster })
}

func (s *Store) ackWhere(match func(*Alert) bool) bool {
	s.mu.Lock()
	now := s.now()
	n := 0
	for _, a := range s.active {
		if match(a) && a.Pending(now, s.snooze) {
			a.AckedAt = now
			n++
		}
	}
	if n > 0 {
		s.dirty = true
	}
	s.mu.Unlock()
	if n > 0 {
		s.publish(Event{Type: EventChanged})
	}
	return n > 0
}

func (s *Store) Get(id string) (Alert, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.history {
		if a.ID == id {
			return *a, true
		}
	}
	return Alert{}, false
}

type Query struct {
	Cluster string
	// Scope: "pending" (default), "active" (unresolved incl. snoozed) or "all" (history).
	Scope string
	Limit int
}

func (s *Store) List(q Query) []Alert {
	s.mu.Lock()
	now := s.now()
	out := make([]Alert, 0, 64)
	for _, a := range s.history {
		if q.Cluster != "" && a.Cluster != q.Cluster {
			continue
		}
		switch q.Scope {
		case "all":
		case "active":
			if a.Resolved {
				continue
			}
		default:
			if !a.Pending(now, s.snooze) {
				continue
			}
		}
		out = append(out, *a)
	}
	s.mu.Unlock()
	slices.SortStableFunc(out, func(x, y Alert) int {
		if x.Resolved != y.Resolved {
			if x.Resolved {
				return 1
			}
			return -1
		}
		if r := y.Severity.Rank() - x.Severity.Rank(); r != 0 && !x.Resolved {
			return r
		}
		return y.LastSeen.Compare(x.LastSeen)
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out
}

func (s *Store) Summary() Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	sum := Summary{BySev: map[Severity]int{}, History: len(s.history), Active: len(s.active)}
	clusters := map[string]bool{}
	for _, a := range s.active {
		if !a.Pending(now, s.snooze) {
			continue
		}
		sum.Pending++
		sum.BySev[a.Severity]++
		clusters[a.Cluster] = true
		if a.Severity.Rank() >= s.nudgeLevel.Rank() {
			sum.Nudgeable++
		}
	}
	sum.Clusters = len(clusters)
	return sum
}

// ActiveByCluster returns pending alert counts keyed by cluster.
func (s *Store) ActiveByCluster() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	m := map[string]int{}
	for _, a := range s.active {
		if a.Pending(now, s.snooze) {
			m[a.Cluster]++
		}
	}
	return m
}

func (s *Store) NudgeLevel() Severity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nudgeLevel
}

func (s *Store) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

func (s *Store) publish(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

type snapshot struct {
	History []*Alert `json:"history"`
}

func (s *Store) Load(path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var snap snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = snap.History
	s.active = map[string]*Alert{}
	for _, a := range s.history {
		if !a.Resolved {
			s.active[a.FullKey()] = a
		}
	}
	return nil
}

func (s *Store) Save(path string) error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	b, err := json.Marshal(snapshot{History: s.history})
	s.dirty = false
	s.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Maintain periodically expires transient alerts and persists history until ctx ends.
func (s *Store) Maintain(ctx context.Context, path string, onErr func(error)) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.ExpireTransient()
			if err := s.Save(path); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}
