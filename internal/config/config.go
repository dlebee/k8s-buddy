package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

type Settings struct {
	Kubeconfig        string   `json:"kubeconfig"`
	ExcludedContexts  []string `json:"excludedContexts"`
	IgnoredNamespaces []string `json:"ignoredNamespaces"`
	MutedKeys         []string `json:"mutedKeys"`
	HistorySize       int      `json:"historySize"`
	SnoozeHours       int      `json:"snoozeHours"`
	NudgeLevel        string   `json:"nudgeLevel"`
	ShowBubble        bool     `json:"showBubble"`
	ShowCharacter     bool     `json:"showCharacter"`
}

func Defaults() Settings {
	return Settings{
		HistorySize:   1000,
		SnoozeHours:   4,
		NudgeLevel:    "notice",
		ShowBubble:    true,
		ShowCharacter: true,
	}
}

func (s Settings) IsExcluded(context string) bool {
	return slices.Contains(s.ExcludedContexts, context)
}

func (s *Settings) normalize() {
	d := Defaults()
	if s.HistorySize <= 0 {
		s.HistorySize = d.HistorySize
	}
	if s.SnoozeHours <= 0 {
		s.SnoozeHours = d.SnoozeHours
	}
	switch s.NudgeLevel {
	case "info", "notice", "warning", "critical":
	default:
		s.NudgeLevel = d.NudgeLevel
	}
	for _, l := range []*[]string{&s.ExcludedContexts, &s.IgnoredNamespaces, &s.MutedKeys} {
		if *l == nil {
			*l = []string{}
		}
		slices.Sort(*l)
		*l = slices.Compact(*l)
	}
}

type Store struct {
	dir  string
	mu   sync.RWMutex
	s    Settings
	subs []func(Settings)
}

func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "k8s-buddy"), nil
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	st := &Store{dir: dir, s: Defaults()}
	b, err := os.ReadFile(st.path())
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &st.s); err != nil {
			return nil, err
		}
	}
	st.s.normalize()
	return st, nil
}

func (st *Store) Dir() string  { return st.dir }
func (st *Store) path() string { return filepath.Join(st.dir, "settings.json") }

func (st *Store) Get() Settings {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.s.clone()
}

func (s Settings) clone() Settings {
	s.ExcludedContexts = slices.Clone(s.ExcludedContexts)
	s.IgnoredNamespaces = slices.Clone(s.IgnoredNamespaces)
	s.MutedKeys = slices.Clone(s.MutedKeys)
	return s
}

func (st *Store) OnChange(fn func(Settings)) {
	st.mu.Lock()
	st.subs = append(st.subs, fn)
	st.mu.Unlock()
}

func (st *Store) Update(fn func(*Settings)) error {
	st.mu.Lock()
	next := st.s.clone()
	fn(&next)
	next.normalize()
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		st.mu.Unlock()
		return err
	}
	tmp := st.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		st.mu.Unlock()
		return err
	}
	if err := os.Rename(tmp, st.path()); err != nil {
		st.mu.Unlock()
		return err
	}
	st.s = next
	subs := slices.Clone(st.subs)
	st.mu.Unlock()
	for _, fn := range subs {
		fn(next)
	}
	return nil
}
