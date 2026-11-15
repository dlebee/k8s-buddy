package alert

import (
	"context"
	"path"
	"slices"
)

// Filter decides whether a candidate alert reaches the user. This is the hook for plugging in
// a model later to suppress deterministic-but-noisy false alarms.
type Filter interface {
	Name() string
	Allow(ctx context.Context, a Alert) bool
}

type namespaceFilter struct{ patterns []string }

// IgnoreNamespaces drops alerts in namespaces matching any glob pattern (e.g. "kube-*").
func IgnoreNamespaces(patterns []string) Filter { return namespaceFilter{slices.Clone(patterns)} }

func (namespaceFilter) Name() string { return "ignored-namespaces" }

func (f namespaceFilter) Allow(_ context.Context, a Alert) bool {
	if a.Namespace == "" {
		return true
	}
	for _, p := range f.patterns {
		if ok, _ := path.Match(p, a.Namespace); ok {
			return false
		}
	}
	return true
}

type clusterFilter struct{ excluded map[string]bool }

// ExcludeClusters drops every alert from the given contexts.
func ExcludeClusters(names []string) Filter {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return clusterFilter{m}
}

func (clusterFilter) Name() string { return "excluded-contexts" }

func (f clusterFilter) Allow(_ context.Context, a Alert) bool { return !f.excluded[a.Cluster] }

type muteFilter struct{ keys map[string]bool }

// Mute drops alerts whose FullKey was muted by the user.
func Mute(keys []string) Filter {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return muteFilter{m}
}

func (muteFilter) Name() string { return "muted" }

func (f muteFilter) Allow(_ context.Context, a Alert) bool { return !f.keys[a.FullKey()] }
