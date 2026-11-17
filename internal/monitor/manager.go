package monitor

import (
	"context"
	"github.com/dlebee/k8s-buddy/internal/history"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dlebee/k8s-buddy/internal/alert"
	"github.com/dlebee/k8s-buddy/internal/config"
	"github.com/dlebee/k8s-buddy/internal/kube"
)

const reloadEvery = 30 * time.Second

type running struct {
	c           *cluster
	cancel      context.CancelFunc
	fingerprint string
}

// Manager keeps one cluster monitor per non-excluded kubeconfig context.
type Manager struct {
	settings *config.Store
	store    *alert.Store
	log      *slog.Logger

	mu       sync.Mutex
	ctx      context.Context
	contexts []kube.Context
	loadErr  string
	running  map[string]*running
	kick     chan struct{}
	doc      doctor
	az       azureState
	hist     *history.Store
}

func NewManager(settings *config.Store, store *alert.Store, log *slog.Logger) *Manager {
	m := &Manager{settings: settings, store: store, log: log, running: map[string]*running{}, kick: make(chan struct{}, 1)}
	settings.OnChange(func(config.Settings) { m.Reload() })
	return m
}

func (m *Manager) Reload() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	t := time.NewTicker(reloadEvery)
	defer t.Stop()
	for {
		m.reconcile()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.kick:
		}
	}
}

func (m *Manager) reconcile() {
	s := m.settings.Get()
	kc, contexts, err := kube.Load(s.Kubeconfig)
	if err == nil && len(contexts) > 0 {
		m.forgetRemovedContexts(s, contexts)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.loadErr = err.Error()
		m.log.Warn("load kubeconfig", "err", err)
		return
	}
	m.loadErr = ""
	m.contexts = contexts

	want := map[string]kube.Context{}
	for _, c := range contexts {
		if !s.IsExcluded(c.Name) {
			want[c.Name] = c
		}
	}
	for name, r := range m.running {
		if w, ok := want[name]; !ok || w.Fingerprint != r.fingerprint {
			r.cancel()
			delete(m.running, name)
			m.store.ResolveCluster(name)
			m.az.mu.Lock()
			delete(m.az.waiting, name)
			m.az.mu.Unlock()
			m.log.Info("stopped monitoring", "context", name)
		}
	}
	for name, kctx := range want {
		if _, ok := m.running[name]; ok {
			continue
		}
		cfg, err := kc.RESTConfig(name)
		cl := newCluster(name, kctx.Server, cfg, m.store)
		cl.azureCLI, cl.onAzureFailure = kctx.AzureCLI, m.onAzureCredentialFailure
		cctx, cancel := context.WithCancel(m.ctx)
		m.running[name] = &running{c: cl, cancel: cancel, fingerprint: kctx.Fingerprint}
		if err != nil {
			cl.onFailure(err)
			continue
		}
		go cl.run(cctx)
		m.log.Info("monitoring", "context", name, "server", kctx.Server)
	}
}

// forgetRemovedContexts drops settings and history that refer to contexts no longer in the kubeconfig.
func (m *Manager) forgetRemovedContexts(s config.Settings, contexts []kube.Context) {
	known := make(map[string]bool, len(contexts))
	names := make([]string, len(contexts))
	for i, c := range contexts {
		known[c.Name] = true
		names[i] = c.Name
	}
	m.store.RetainClusters(append(names, AzureCluster))
	gone := func(cluster string) bool { return !known[cluster] }
	stale := slices.ContainsFunc(s.ExcludedContexts, gone) ||
		slices.ContainsFunc(s.MutedKeys, func(k string) bool { return gone(strings.SplitN(k, "|", 2)[0]) })
	if !stale {
		return
	}
	err := m.settings.Update(func(st *config.Settings) {
		st.ExcludedContexts = slices.DeleteFunc(st.ExcludedContexts, gone)
		st.MutedKeys = slices.DeleteFunc(st.MutedKeys, func(k string) bool { return gone(strings.SplitN(k, "|", 2)[0]) })
	})
	if err != nil {
		m.log.Warn("prune settings", "err", err)
	} else {
		m.log.Info("forgot contexts removed from kubeconfig")
	}
}

type Overview struct {
	KubeconfigError string        `json:"kubeconfigError,omitempty"`
	Clusters        []ClusterInfo `json:"clusters"`
}

func (m *Manager) Overview() Overview {
	s := m.settings.Get()
	m.mu.Lock()
	contexts := m.contexts
	ov := Overview{KubeconfigError: m.loadErr}
	clusters := make([]*cluster, len(contexts))
	for i, c := range contexts {
		if r, ok := m.running[c.Name]; ok {
			clusters[i] = r.c
		}
	}
	m.mu.Unlock()

	counts := m.store.ActiveByCluster()
	ov.Clusters = make([]ClusterInfo, len(contexts))
	var wg sync.WaitGroup
	for i, c := range contexts {
		if s.IsExcluded(c.Name) || clusters[i] == nil {
			ov.Clusters[i] = ClusterInfo{Name: c.Name, Server: c.Server, Status: StatusExcluded}
			continue
		}
		wg.Go(func() {
			ci := clusters[i].info()
			ci.Alerts = counts[c.Name]
			ov.Clusters[i] = ci
		})
	}
	wg.Wait()
	return ov
}
