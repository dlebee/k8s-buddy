package monitor

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	networkinglisters "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/dlebee/k8s-buddy/internal/alert"
)

type Status string

const (
	StatusConnecting Status = "connecting"
	StatusSyncing    Status = "syncing"
	StatusConnected  Status = "connected"
	StatusError      Status = "error"
	StatusExcluded   Status = "excluded"
)

const (
	sweepEvery   = 20 * time.Second
	versionEvery = 5 * time.Minute
	syncTimeout  = 2 * time.Minute
)

type Stats struct {
	Nodes       int `json:"nodes"`
	NodesReady  int `json:"nodesReady"`
	Pods        int `json:"pods"`
	PodsRunning int `json:"podsRunning"`
	PodsDone    int `json:"podsDone"`
	PodsProblem int `json:"podsProblem"`
	PVCs        int `json:"pvcs"`
	PVCPending  int `json:"pvcPending"`
	LBs         int `json:"lbs"`
	LBPending   int `json:"lbPending"`
	Warnings1h  int `json:"warnings1h"`
}

type ClusterInfo struct {
	Name           string            `json:"name"`
	Server         string            `json:"server"`
	Status         Status            `json:"status"`
	Error          string            `json:"error,omitempty"`
	Version        string            `json:"version,omitempty"`
	ConnectedAt    time.Time         `json:"connectedAt,omitzero"`
	ResourceErrors map[string]string `json:"resourceErrors,omitempty"`
	Stats          *Stats            `json:"stats,omitempty"`
	Alerts         int               `json:"alerts"`
}

type resourceErr struct {
	msg string
	at  time.Time
}

type cluster struct {
	name      string
	server    string
	cfg       *rest.Config
	store     *alert.Store
	startedAt time.Time

	mu          sync.Mutex
	status      Status
	lastErr     string
	version     string
	connectedAt time.Time
	everUp      bool
	failures    int
	resErrs     map[string]resourceErr
	pools       map[string]map[string]map[string]bool
	stats       *Stats
	statsAt     time.Time

	informers map[string]cache.SharedIndexInformer
	pods      corelisters.PodLister
	pvcs      corelisters.PersistentVolumeClaimLister
	services  corelisters.ServiceLister
	nodes     corelisters.NodeLister
	events    corelisters.EventLister
	deploys   appslisters.DeploymentLister
	sts       appslisters.StatefulSetLister
	ds        appslisters.DaemonSetLister
	cs        kubernetes.Interface
	retry     chan struct{}
	azureCLI  bool
	// onAzureFailure reports whether an expired Azure CLI session explains a credential error.
	onAzureFailure func(*cluster) bool
	runCtx         context.Context
	ingresses      networkinglisters.IngressLister
	ingressClasses networkinglisters.IngressClassLister
	helm           cache.GenericLister
	dynf           dynamicinformer.DynamicSharedInformerFactory
	dynListers     map[string]cache.GenericLister
	disc           discoveryCache
	tables         tableCache
	configmaps     corelisters.ConfigMapLister
	nodesSynced    atomic.Bool
}

func newCluster(name, server string, cfg *rest.Config, store *alert.Store) *cluster {
	return &cluster{
		name:      name,
		server:    server,
		cfg:       cfg,
		store:     store,
		startedAt: time.Now(),
		status:    StatusConnecting,
		retry:     make(chan struct{}, 1),
		resErrs:   map[string]resourceErr{},
		pools:     map[string]map[string]map[string]bool{},
	}
}

func (c *cluster) run(ctx context.Context) {
	backoff := 15 * time.Second
	for {
		v, err := c.serverVersion()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			c.onVersion(v)
			break
		}
		c.onFailure(err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		case <-c.retry:
			backoff = 15 * time.Second
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
	cs, err := kubernetes.NewForConfig(c.cfg)
	if err != nil {
		c.onFailure(err)
		return
	}
	c.startInformers(ctx, cs)

	sweep := time.NewTicker(sweepEvery)
	defer sweep.Stop()
	ver := time.NewTicker(versionEvery)
	defer ver.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			c.sweep()
		case <-ver.C:
			c.pollVersion(ctx)
		case <-c.retry:
			c.pollVersion(ctx)
		}
	}
}

func (c *cluster) pollVersion(ctx context.Context) {
	v, err := c.serverVersion()
	switch {
	case ctx.Err() != nil:
	case err != nil:
		c.onFailure(err)
	default:
		c.onVersion(v)
		c.ensureCRDWatchers()
	}
}

func (c *cluster) serverVersion() (string, error) {
	cfg := rest.CopyConfig(c.cfg)
	cfg.Timeout = 20 * time.Second
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return "", err
	}
	v, err := dc.ServerVersion()
	if err != nil {
		return "", err
	}
	return v.GitVersion, nil
}

func (c *cluster) onVersion(v string) {
	c.mu.Lock()
	prev := c.version
	c.version = v
	c.failures = 0
	c.lastErr = ""
	if !c.everUp {
		c.everUp = true
		c.connectedAt = time.Now()
	}
	if c.status == StatusConnecting || c.status == StatusError || c.status == StatusSignIn {
		if c.informers == nil {
			c.status = StatusSyncing
		} else {
			c.status = StatusConnected
		}
	}
	c.mu.Unlock()
	c.store.ResolveMissing(c.name, "connectivity", nil)
	if prev != "" && prev != v {
		c.store.Record(alert.Alert{
			Key:      "cp-version/" + v,
			Source:   "version",
			Cluster:  c.name,
			Severity: alert.Notice,
			Category: "upgrade",
			Title:    fmt.Sprintf("Control plane upgraded %s → %s", prev, v),
			Hint:     kubectl(c.name, "", "version"),
		})
	}
	c.checkEOL(v)
}

func (c *cluster) checkEOL(v string) {
	e, ok := lookupEOL(v)
	if !ok {
		c.store.ResolveMissing(c.name, "k8s-eol", nil)
		return
	}
	now := time.Now()
	days := int(e.EOL.Sub(now).Hours() / 24)
	if days > 60 {
		c.store.ResolveMissing(c.name, "k8s-eol", nil)
		return
	}
	sev := alert.Notice
	title := fmt.Sprintf("Kubernetes %s reaches upstream EOL in %d days (%s)", e.Minor, days, e.EOL.Format("2006-01-02"))
	if days < 0 {
		sev = alert.Warning
		title = fmt.Sprintf("Kubernetes %s is past upstream EOL (%s)", e.Minor, e.EOL.Format("2006-01-02"))
	}
	detail := "Latest patch upstream is " + e.LatestPatch + ". Managed distros (EKS/GKE/AKS) usually extend support — check your provider."
	c.store.Observe(alert.Alert{
		Key:      "k8s-eol/" + e.Minor,
		Source:   "k8s-eol",
		Cluster:  c.name,
		Severity: sev,
		Category: "upgrade",
		Title:    title,
		Detail:   detail,
		Hint:     kubectl(c.name, "", "version"),
	})
}

func (c *cluster) onFailure(err error) {
	if c.azureCLI && c.onAzureFailure != nil && isCredentialError(err) && c.onAzureFailure(c) {
		c.mu.Lock()
		c.failures++
		c.lastErr = "waiting for Azure sign-in"
		c.mu.Unlock()
		return
	}
	c.mu.Lock()
	c.failures++
	c.lastErr = truncate(err.Error(), 400)
	everUp := c.everUp
	failures := c.failures
	if !everUp || failures >= 2 {
		c.status = StatusError
	}
	c.mu.Unlock()
	if everUp && failures < 2 {
		return
	}
	sev, title := alert.Notice, "Can't reach "+c.name
	if everUp {
		sev, title = alert.Warning, "Lost connection to "+c.name
	}
	c.store.Observe(alert.Alert{
		Key:      "unreachable",
		Source:   "connectivity",
		Cluster:  c.name,
		Severity: sev,
		Category: "connectivity",
		Title:    title,
		Detail:   truncate(err.Error(), 400),
		Hint:     "If this context is stale, exclude it in Settings.\n" + kubectl(c.name, "", "cluster-info"),
	})
}

func stripManagedFields(obj any) (any, error) {
	if acc, err := meta.Accessor(obj); err == nil {
		acc.SetManagedFields(nil)
	}
	return obj, nil
}

func (c *cluster) startInformers(ctx context.Context, cs kubernetes.Interface) {
	tr := informers.WithTransform(stripManagedFields)
	f := informers.NewSharedInformerFactoryWithOptions(cs, 0, tr)
	ef := informers.NewSharedInformerFactoryWithOptions(cs, 0, tr,
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.FieldSelector = "type=Warning" }))

	pods := f.Core().V1().Pods()
	pvcs := f.Core().V1().PersistentVolumeClaims()
	svcs := f.Core().V1().Services()
	nodes := f.Core().V1().Nodes()
	events := ef.Core().V1().Events()
	deploys := f.Apps().V1().Deployments()
	sts := f.Apps().V1().StatefulSets()
	ds := f.Apps().V1().DaemonSets()
	ings := f.Networking().V1().Ingresses()
	ics := f.Networking().V1().IngressClasses()

	infs := map[string]cache.SharedIndexInformer{
		"pods":           pods.Informer(),
		"pvcs":           pvcs.Informer(),
		"services":       svcs.Informer(),
		"nodes":          nodes.Informer(),
		"events":         events.Informer(),
		"deployments":    deploys.Informer(),
		"statefulsets":   sts.Informer(),
		"daemonsets":     ds.Informer(),
		"ingresses":      ings.Informer(),
		"ingressclasses": ics.Informer(),
	}
	var mf metadatainformer.SharedInformerFactory
	if mc, err := metadata.NewForConfig(c.cfg); err == nil {
		mf = metadatainformer.NewFilteredSharedInformerFactory(mc, 0, "", func(o *metav1.ListOptions) { o.LabelSelector = "owner=helm" })
		h := mf.ForResource(corev1.SchemeGroupVersion.WithResource("secrets"))
		_ = h.Informer().SetTransform(stripHelmMeta)
		infs["helm"] = h.Informer()
		c.helm = h.Lister()
	}
	for name, inf := range infs {
		_ = inf.SetWatchErrorHandler(func(_ *cache.Reflector, err error) { c.setResourceErr(name, err) })
	}

	_, _ = infs["pods"].AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(o, n any) {
			op, ok1 := o.(*corev1.Pod)
			np, ok2 := n.(*corev1.Pod)
			if ok1 && ok2 && op.ResourceVersion != np.ResourceVersion {
				c.onPodUpdate(op, np)
			}
		},
	})
	_, _ = infs["nodes"].AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(o any) {
			if n, ok := o.(*corev1.Node); ok && c.nodesSynced.Load() {
				c.onNodeAdd(n)
			}
		},
		UpdateFunc: func(_, o any) {
			if n, ok := o.(*corev1.Node); ok && c.nodesSynced.Load() {
				c.checkNodeInfo(n, false)
			}
		},
		DeleteFunc: func(o any) {
			if t, ok := o.(cache.DeletedFinalStateUnknown); ok {
				o = t.Obj
			}
			if n, ok := o.(*corev1.Node); ok && c.nodesSynced.Load() {
				c.onNodeDelete(n)
			}
		},
	})
	_, _ = infs["events"].AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(o any) {
			if e, ok := o.(*corev1.Event); ok {
				c.onEvent(e)
			}
		},
		UpdateFunc: func(o, n any) {
			oe, ok1 := o.(*corev1.Event)
			ne, ok2 := n.(*corev1.Event)
			if ok1 && ok2 && oe.ResourceVersion != ne.ResourceVersion {
				c.onEvent(ne)
			}
		},
	})

	c.mu.Lock()
	c.informers = infs
	c.cs = cs
	c.runCtx = ctx
	c.pods, c.pvcs, c.services, c.nodes, c.events = pods.Lister(), pvcs.Lister(), svcs.Lister(), nodes.Lister(), events.Lister()
	c.deploys, c.sts, c.ds = deploys.Lister(), sts.Lister(), ds.Lister()
	c.ingresses = ings.Lister()
	c.ingressClasses = ics.Lister()
	c.mu.Unlock()

	f.Start(ctx.Done())
	ef.Start(ctx.Done())
	if mf != nil {
		mf.Start(ctx.Done())
	}

	go func() {
		sctx, cancel := context.WithTimeout(ctx, syncTimeout)
		defer cancel()
		if cache.WaitForCacheSync(sctx.Done(), infs["nodes"].HasSynced) {
			c.seedNodePools()
			c.nodesSynced.Store(true)
		}
		cache.WaitForCacheSync(sctx.Done(), infs["pods"].HasSynced, infs["pvcs"].HasSynced, infs["services"].HasSynced)
		if ctx.Err() != nil {
			return
		}
		c.mu.Lock()
		if c.status == StatusSyncing {
			c.status = StatusConnected
		}
		c.mu.Unlock()
		c.ensureCRDWatchers()
		c.sweep()
	}()
}

func (c *cluster) setResourceErr(resource string, err error) {
	c.mu.Lock()
	c.resErrs[resource] = resourceErr{msg: truncate(err.Error(), 300), at: time.Now()}
	c.mu.Unlock()
}

// ensureLazy starts informers for kinds we only watch once someone asks for them.
func (c *cluster) ensureLazy(resource string) {
	c.mu.Lock()
	if c.informers == nil || c.informers[resource] != nil || c.runCtx == nil {
		c.mu.Unlock()
		return
	}
	var inf cache.SharedIndexInformer
	var f informers.SharedInformerFactory
	switch resource {
	case "configmaps":
		f = informers.NewSharedInformerFactoryWithOptions(c.cs, 0, informers.WithTransform(stripManagedFields))
		cm := f.Core().V1().ConfigMaps()
		inf, c.configmaps = cm.Informer(), cm.Lister()
	default:
		c.mu.Unlock()
		return
	}
	_ = inf.SetWatchErrorHandler(func(_ *cache.Reflector, err error) { c.setResourceErr(resource, err) })
	c.informers[resource] = inf
	ctx := c.runCtx
	c.mu.Unlock()
	f.Start(ctx.Done())
}

func (c *cluster) resourceError(resource string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.resErrs[resource]; ok {
		return e.msg
	}
	return ""
}

func (c *cluster) synced(resource string) bool {
	c.mu.Lock()
	inf := c.informers[resource]
	c.mu.Unlock()
	return inf != nil && inf.HasSynced()
}

func (c *cluster) info() ClusterInfo {
	ci := ClusterInfo{Name: c.name, Server: c.server}
	c.mu.Lock()
	ci.Status, ci.Error, ci.Version, ci.ConnectedAt = c.status, c.lastErr, c.version, c.connectedAt
	cutoff := time.Now().Add(-3 * time.Minute)
	for r, e := range c.resErrs {
		if e.at.After(cutoff) {
			if ci.ResourceErrors == nil {
				ci.ResourceErrors = map[string]string{}
			}
			ci.ResourceErrors[r] = e.msg
		}
	}
	c.mu.Unlock()
	if ci.Status == StatusConnected || ci.Status == StatusSyncing {
		ci.Stats = c.computeStats()
	}
	return ci
}

func (c *cluster) computeStats() *Stats {
	c.mu.Lock()
	if c.stats != nil && time.Since(c.statsAt) < 5*time.Second {
		s := *c.stats
		c.mu.Unlock()
		return &s
	}
	c.mu.Unlock()

	s := &Stats{}
	if c.synced("nodes") {
		nodes, _ := c.nodes.List(labels.Everything())
		s.Nodes = len(nodes)
		for _, n := range nodes {
			if cond := nodeCondition(n, corev1.NodeReady); cond != nil && cond.Status == corev1.ConditionTrue {
				s.NodesReady++
			}
		}
	}
	if c.synced("pods") {
		pods, _ := c.pods.List(labels.Everything())
		s.Pods = len(pods)
		for _, p := range pods {
			switch p.Status.Phase {
			case corev1.PodRunning:
				s.PodsRunning++
			case corev1.PodSucceeded:
				s.PodsDone++
			}
			if podProblem(p) != "" {
				s.PodsProblem++
			}
		}
	}
	if c.synced("pvcs") {
		pvcs, _ := c.pvcs.List(labels.Everything())
		s.PVCs = len(pvcs)
		for _, p := range pvcs {
			if p.Status.Phase != corev1.ClaimBound {
				s.PVCPending++
			}
		}
	}
	if c.synced("services") {
		svcs, _ := c.services.List(labels.Everything())
		for _, sv := range svcs {
			if sv.Spec.Type == corev1.ServiceTypeLoadBalancer {
				s.LBs++
				if len(sv.Status.LoadBalancer.Ingress) == 0 {
					s.LBPending++
				}
			}
		}
	}
	if c.synced("events") {
		evs, _ := c.events.List(labels.Everything())
		cutoff := time.Now().Add(-time.Hour)
		for _, e := range evs {
			if eventTime(e).After(cutoff) {
				s.Warnings1h++
			}
		}
	}
	c.mu.Lock()
	c.stats, c.statsAt = s, time.Now()
	c.mu.Unlock()
	cp := *s
	return &cp
}
