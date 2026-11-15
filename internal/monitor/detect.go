package monitor

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/dlebee/k8s-buddy/internal/alert"
)

const (
	pvcPendingAfter   = 2 * time.Minute
	lbPendingAfter    = 3 * time.Minute
	unschedulableFor  = 5 * time.Minute
	nodeNotReadyGrace = 3 * time.Minute
	eventMaxAge       = 10 * time.Minute
)

var badWaiting = map[string]bool{
	"CrashLoopBackOff":           true,
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"InvalidImageName":           true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"RunContainerError":          true,
}

// Warning event reasons worth surfacing. Reasons already covered by state detectors
// (BackOff, FailedScheduling, NodeNotReady) are left out to avoid double alerts.
var eventReasons = map[string]alert.Severity{
	"FailedMount":                      alert.Warning,
	"FailedAttachVolume":               alert.Warning,
	"FailedMapVolume":                  alert.Warning,
	"ProvisioningFailed":               alert.Warning,
	"FailedBinding":                    alert.Warning,
	"FailedCreatePodSandBox":           alert.Warning,
	"Evicted":                          alert.Warning,
	"Preempted":                        alert.Notice,
	"OOMKilling":                       alert.Warning,
	"SystemOOM":                        alert.Warning,
	"Rebooted":                         alert.Warning,
	"EvictionThresholdMet":             alert.Warning,
	"FreeDiskSpaceFailed":              alert.Warning,
	"NetworkNotReady":                  alert.Warning,
	"FailedCreate":                     alert.Warning,
	"FailedDaemonPod":                  alert.Warning,
	"BackoffLimitExceeded":             alert.Warning,
	"DeadlineExceeded":                 alert.Warning,
	"SyncLoadBalancerFailed":           alert.Warning,
	"FailedToUpdateEndpoint":           alert.Notice,
	"ImageGCFailed":                    alert.Info,
	"ContainerGCFailed":                alert.Info,
	"FailedKillPod":                    alert.Info,
	"FailedPreStopHook":                alert.Info,
	"FailedPostStartHook":              alert.Warning,
	"InvalidDiskCapacity":              alert.Info,
	"FailedNodeAllocatableEnforcement": alert.Info,
	"Unhealthy":                        alert.Warning,
	// node-problem-detector (AKS and others): platform maintenance and node OS trouble.
	"VMEventScheduled":            alert.Notice,
	"FreezeScheduled":             alert.Notice,
	"RebootScheduled":             alert.Notice,
	"RedeployScheduled":           alert.Notice,
	"TerminateScheduled":          alert.Notice,
	"PreemptScheduled":            alert.Warning,
	"KernelDeadlock":              alert.Warning,
	"ReadonlyFilesystem":          alert.Critical,
	"FilesystemCorruptionProblem": alert.Warning,
	"FrequentKubeletRestart":      alert.Warning,
	"FrequentContainerdRestart":   alert.Warning,
}

// Scheduled maintenance stays relevant until it happens, so these are surfaced even if they
// were emitted before k8s-buddy started.
var maintenanceReasons = map[string]string{
	"VMEventScheduled":   "Upcoming VM maintenance",
	"FreezeScheduled":    "VM freeze scheduled",
	"RebootScheduled":    "VM reboot scheduled",
	"RedeployScheduled":  "VM redeploy scheduled",
	"TerminateScheduled": "VM termination scheduled",
	"PreemptScheduled":   "Spot VM eviction scheduled",
}

const (
	maintenanceMaxAge = 12 * time.Hour
	unhealthyMinCount = 3
)

func eventCount(e *corev1.Event) int32 {
	if e.Series != nil && e.Series.Count > 0 {
		return e.Series.Count
	}
	return max(e.Count, 1)
}

// sweep re-evaluates all state-based detectors and resolves alerts whose condition cleared.
func (c *cluster) sweep() {
	if c.synced("pods") {
		c.emit("pods", c.detectPods())
	}
	if c.synced("pvcs") {
		c.emit("pvcs", c.detectPVCs())
	}
	if c.synced("services") {
		c.emit("services", c.detectServices())
	}
	if c.synced("nodes") {
		c.emit("nodes", c.detectNodes())
	}
	if c.synced("ingresses") {
		c.emit("ingresses", c.detectIngresses())
	}
	if c.synced("helm") {
		c.emit("helm", c.detectHelm())
	}
	if c.synced("cm-certificates") {
		c.emit("cert-manager", c.detectCertManager())
	}
}

func (c *cluster) emit(source string, alerts []alert.Alert) {
	present := make(map[string]bool, len(alerts))
	for _, a := range alerts {
		a.Source, a.Cluster = source, c.name
		present[a.Key] = true
		c.store.Observe(a)
	}
	c.store.ResolveMissing(c.name, source, present)
}

// podProblem returns the waiting reason that makes a pod unhealthy, if any.
func podProblem(p *corev1.Pod) string {
	if p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
		return ""
	}
	for _, cs := range slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses) {
		if w := cs.State.Waiting; w != nil && badWaiting[w.Reason] {
			return w.Reason
		}
	}
	return ""
}

type podGroup struct {
	a    alert.Alert
	pods []string
}

func (c *cluster) detectPods() []alert.Alert {
	pods, _ := c.pods.List(labels.Everything())
	groups := map[string]*podGroup{}
	now := time.Now()
	add := func(key string, p *corev1.Pod, mk func() alert.Alert) {
		g, ok := groups[key]
		if !ok {
			g = &podGroup{a: mk()}
			g.a.Key = key
			groups[key] = g
		}
		g.pods = append(g.pods, p.Name)
	}
	for _, p := range pods {
		kind, wl := workloadOf(p)
		ref := shortRef(kind, wl)
		if reason := podProblem(p); reason != "" {
			var container, msg string
			for _, cs := range slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses) {
				if w := cs.State.Waiting; w != nil && w.Reason == reason {
					container, msg = cs.Name, w.Message
					break
				}
			}
			key := fmt.Sprintf("pod-waiting/%s/%s/%s/%s", p.Namespace, kind, wl, reason)
			add(key, p, func() alert.Alert {
				return alert.Alert{
					Severity: alert.Warning, Category: "pod", Kind: kind, Namespace: p.Namespace, Name: wl,
					Title:  fmt.Sprintf("%s · %s", reason, ref),
					Detail: joinNonEmpty(" · ", "container "+container, truncate(msg, 240)),
					Hint: strings.Join([]string{
						kubectl(c.name, p.Namespace, "describe pod "+p.Name),
						kubectl(c.name, p.Namespace, fmt.Sprintf("logs %s -c %s --previous", p.Name, container)),
					}, "\n"),
				}
			})
			continue
		}
		if p.Status.Phase == corev1.PodPending && p.DeletionTimestamp == nil {
			for _, cond := range p.Status.Conditions {
				if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse &&
					cond.Reason == corev1.PodReasonUnschedulable && now.Sub(cond.LastTransitionTime.Time) > unschedulableFor {
					key := fmt.Sprintf("pod-unschedulable/%s/%s/%s", p.Namespace, kind, wl)
					add(key, p, func() alert.Alert {
						return alert.Alert{
							Severity: alert.Warning, Category: "scheduling", Kind: kind, Namespace: p.Namespace, Name: wl,
							Title:  "Unschedulable · " + ref,
							Detail: truncate(cond.Message, 300),
							Hint:   kubectl(c.name, p.Namespace, "describe pod "+p.Name),
						}
					})
				}
			}
		}
	}
	out := make([]alert.Alert, 0, len(groups))
	for _, g := range groups {
		if n := len(g.pods); n > 1 {
			g.a.Title += fmt.Sprintf(" (%d pods)", n)
		}
		g.a.Count = len(g.pods)
		out = append(out, g.a)
	}
	return out
}

func (c *cluster) detectPVCs() []alert.Alert {
	pvcs, _ := c.pvcs.List(labels.Everything())
	var out []alert.Alert
	for _, p := range pvcs {
		if p.DeletionTimestamp != nil {
			continue
		}
		sc := "<default>"
		if p.Spec.StorageClassName != nil {
			sc = *p.Spec.StorageClassName
		}
		switch {
		case p.Status.Phase == corev1.ClaimLost:
			out = append(out, alert.Alert{
				Key: "pvc-lost/" + p.Namespace + "/" + p.Name, Severity: alert.Critical, Category: "storage",
				Kind: "PersistentVolumeClaim", Namespace: p.Namespace, Name: p.Name,
				Title:  "PVC lost its volume · pvc/" + p.Name,
				Detail: "storageClass " + sc,
				Hint:   kubectl(c.name, p.Namespace, "describe pvc "+p.Name),
			})
		case p.Status.Phase == corev1.ClaimPending && time.Since(p.CreationTimestamp.Time) > pvcPendingAfter:
			out = append(out, alert.Alert{
				Key: "pvc-pending/" + p.Namespace + "/" + p.Name, Severity: alert.Warning, Category: "storage",
				Kind: "PersistentVolumeClaim", Namespace: p.Namespace, Name: p.Name,
				Title:  "PVC stuck Pending · pvc/" + p.Name,
				Detail: fmt.Sprintf("storageClass %s · pending for %s", sc, ago(p.CreationTimestamp.Time)),
				Hint:   kubectl(c.name, p.Namespace, "describe pvc "+p.Name),
			})
		}
	}
	return out
}

func (c *cluster) detectServices() []alert.Alert {
	svcs, _ := c.services.List(labels.Everything())
	var out []alert.Alert
	for _, s := range svcs {
		if s.Spec.Type != corev1.ServiceTypeLoadBalancer || len(s.Status.LoadBalancer.Ingress) > 0 ||
			s.DeletionTimestamp != nil || time.Since(s.CreationTimestamp.Time) < lbPendingAfter {
			continue
		}
		detail := "waiting for " + ago(s.CreationTimestamp.Time)
		if s.Spec.LoadBalancerClass != nil {
			detail += " · class " + *s.Spec.LoadBalancerClass
		}
		out = append(out, alert.Alert{
			Key: "lb-pending/" + s.Namespace + "/" + s.Name, Severity: alert.Warning, Category: "network",
			Kind: "Service", Namespace: s.Namespace, Name: s.Name,
			Title:  "LoadBalancer has no external IP · svc/" + s.Name,
			Detail: detail,
			Hint:   kubectl(c.name, s.Namespace, "describe svc "+s.Name),
		})
	}
	return out
}

var pressureConditions = []corev1.NodeConditionType{
	corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure, corev1.NodeNetworkUnavailable,
}

func (c *cluster) detectNodes() []alert.Alert {
	nodes, _ := c.nodes.List(labels.Everything())
	var out []alert.Alert
	cordoned := map[string][]string{}
	versions := map[string]map[string]map[string]int{}
	for _, n := range nodes {
		pool := poolOf(n)
		if cond := nodeCondition(n, corev1.NodeReady); cond == nil || cond.Status != corev1.ConditionTrue {
			if time.Since(n.CreationTimestamp.Time) > nodeNotReadyGrace {
				detail := "no Ready condition reported"
				if cond != nil {
					detail = joinNonEmpty(" · ", cond.Reason, truncate(cond.Message, 200), "since "+ago(cond.LastTransitionTime.Time))
				}
				out = append(out, alert.Alert{
					Key: "node-notready/" + n.Name, Severity: alert.Critical, Category: "node", Kind: "Node", Name: n.Name,
					Title: "Node NotReady · " + n.Name, Detail: detail,
					Hint: kubectl(c.name, "", "describe node "+n.Name),
				})
			}
		}
		for _, t := range pressureConditions {
			if cond := nodeCondition(n, t); cond != nil && cond.Status == corev1.ConditionTrue {
				out = append(out, alert.Alert{
					Key: fmt.Sprintf("node-%s/%s", strings.ToLower(string(t)), n.Name), Severity: alert.Warning,
					Category: "node", Kind: "Node", Name: n.Name,
					Title:  fmt.Sprintf("Node %s · %s", t, n.Name),
					Detail: truncate(cond.Message, 240),
					Hint:   kubectl(c.name, "", "describe node "+n.Name),
				})
			}
		}
		if n.Spec.Unschedulable {
			cordoned[pool] = append(cordoned[pool], n.Name)
		}
		pv := versions[pool]
		if pv == nil {
			pv = map[string]map[string]int{"kubelet": {}, "os image": {}}
			versions[pool] = pv
		}
		pv["kubelet"][n.Status.NodeInfo.KubeletVersion]++
		pv["os image"][n.Status.NodeInfo.OSImage]++
	}
	for pool, names := range cordoned {
		slices.Sort(names)
		out = append(out, alert.Alert{
			Key: "node-cordoned/" + pool, Severity: alert.Info, Category: "node", Kind: "Node", Count: len(names),
			Title:  fmt.Sprintf("%d node(s) cordoned in pool %s", len(names), pool),
			Detail: truncate(strings.Join(names, ", "), 300),
			Hint:   kubectl(c.name, "", "get nodes -o wide"),
		})
	}
	for pool, fields := range versions {
		for _, field := range []string{"kubelet", "os image"} {
			vals := fields[field]
			if len(vals) < 2 {
				continue
			}
			keys := slices.Sorted(maps.Keys(vals))
			parts := make([]string, len(keys))
			for i, k := range keys {
				parts[i] = fmt.Sprintf("%s ×%d", k, vals[k])
			}
			out = append(out, alert.Alert{
				Key: "node-skew/" + pool + "/" + field, Severity: alert.Notice, Category: "upgrade", Kind: "Node",
				Title:  fmt.Sprintf("Pool %s has mixed %s versions (rolling update?)", pool, field),
				Detail: strings.Join(parts, " · "),
				Hint:   kubectl(c.name, "", "get nodes -o wide"),
			})
		}
	}
	return out
}

func (c *cluster) onPodUpdate(old, cur *corev1.Pod) {
	prev := map[string]int32{}
	for _, cs := range slices.Concat(old.Status.InitContainerStatuses, old.Status.ContainerStatuses) {
		prev[cs.Name] = cs.RestartCount
	}
	for _, cs := range slices.Concat(cur.Status.InitContainerStatuses, cur.Status.ContainerStatuses) {
		before, ok := prev[cs.Name]
		if !ok || cs.RestartCount <= before {
			continue
		}
		reason, detail := "restarted", ""
		if t := cs.LastTerminationState.Terminated; t != nil {
			if t.Reason != "" {
				reason = t.Reason
			}
			detail = joinNonEmpty(" · ", fmt.Sprintf("exit code %d", t.ExitCode), truncate(t.Message, 200))
		}
		kind, wl := workloadOf(cur)
		c.store.Record(alert.Alert{
			Key:      fmt.Sprintf("restart/%s/%s/%s/%s", cur.Namespace, kind, wl, cs.Name),
			Source:   "restarts",
			Cluster:  c.name,
			Severity: alert.Warning,
			Category: "restart",
			Kind:     kind, Namespace: cur.Namespace, Name: wl,
			Count:  int(cs.RestartCount - before),
			Title:  fmt.Sprintf("Container %s restarted (%s) · %s", cs.Name, reason, shortRef(kind, wl)),
			Detail: joinNonEmpty(" · ", "pod "+cur.Name, detail, fmt.Sprintf("%d restarts total", cs.RestartCount)),
			Hint:   kubectl(c.name, cur.Namespace, fmt.Sprintf("logs %s -c %s --previous", cur.Name, cs.Name)),
		})
	}
}

func eventTime(e *corev1.Event) time.Time {
	switch {
	case e.Series != nil && !e.Series.LastObservedTime.IsZero():
		return e.Series.LastObservedTime.Time
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	default:
		return e.CreationTimestamp.Time
	}
}

func (c *cluster) onEvent(e *corev1.Event) {
	sev, ok := eventReasons[e.Reason]
	if !ok {
		return
	}
	ts := eventTime(e)
	maintenance, isMaintenance := maintenanceReasons[e.Reason]
	switch {
	case isMaintenance:
		if time.Since(ts) > maintenanceMaxAge {
			return
		}
	case ts.Before(c.startedAt.Add(-time.Minute)) || time.Since(ts) > eventMaxAge:
		return
	}
	// One failed probe during a rollout is normal; keep repeated failures.
	if e.Reason == "Unhealthy" && eventCount(e) < unhealthyMinCount {
		return
	}
	obj := e.InvolvedObject
	kind, name := obj.Kind, obj.Name
	if kind == "Pod" && c.pods != nil {
		if p, err := c.pods.Pods(obj.Namespace).Get(obj.Name); err == nil {
			kind, name = workloadOf(p)
		}
	}
	if kind == "Job" {
		kind, name = jobOwner(name)
	}
	target := strings.ToLower(obj.Kind) + "/" + obj.Name
	title := fmt.Sprintf("%s · %s", e.Reason, shortRef(kind, name))
	category := "event"
	switch {
	case isMaintenance:
		title, category = maintenance+" · "+shortRef(kind, name), "maintenance"
	case e.Reason == "Unhealthy":
		probe := "Probe"
		if i := strings.Index(e.Message, " probe failed"); i > 0 {
			probe = e.Message[:i] + " probe"
		}
		title = probe + " failing · " + shortRef(kind, name)
	}
	c.store.Record(alert.Alert{
		Key:      fmt.Sprintf("event/%s/%s/%s/%s", e.Reason, obj.Namespace, kind, name),
		Source:   "events",
		Cluster:  c.name,
		Severity: sev,
		Category: category,
		Kind:     kind, Namespace: obj.Namespace, Name: name,
		Title:  title,
		Detail: truncate(e.Message, 300),
		Hint:   kubectl(c.name, obj.Namespace, "describe "+target),
	})
}

var trackedNodeInfo = []struct {
	field string
	get   func(corev1.NodeSystemInfo) string
}{
	{"kubelet", func(i corev1.NodeSystemInfo) string { return i.KubeletVersion }},
	{"os image", func(i corev1.NodeSystemInfo) string { return i.OSImage }},
}

func (c *cluster) seedNodePools() {
	nodes, _ := c.nodes.List(labels.Everything())
	for _, n := range nodes {
		c.checkNodeInfo(n, true)
	}
}

// checkNodeInfo flags kubelet / OS image values not previously seen in the node's pool,
// which is what both in-place upgrades and node-replacement upgrades look like.
func (c *cluster) checkNodeInfo(n *corev1.Node, seeding bool) {
	pool := poolOf(n)
	type change struct{ field, val, was string }
	var changes []change
	c.mu.Lock()
	pv := c.pools[pool]
	if pv == nil {
		pv = map[string]map[string]bool{}
		c.pools[pool] = pv
	}
	for _, t := range trackedNodeInfo {
		val := t.get(n.Status.NodeInfo)
		if val == "" {
			continue
		}
		seen := pv[t.field]
		if seen == nil {
			seen = map[string]bool{}
			pv[t.field] = seen
		}
		if !seeding && len(seen) > 0 && !seen[val] {
			changes = append(changes, change{t.field, val, strings.Join(slices.Sorted(maps.Keys(seen)), ", ")})
		}
		seen[val] = true
	}
	c.mu.Unlock()
	for _, ch := range changes {
		c.store.Record(alert.Alert{
			Key:      fmt.Sprintf("node-update/%s/%s/%s", pool, ch.field, ch.val),
			Source:   "node-updates",
			Cluster:  c.name,
			Severity: alert.Notice,
			Category: "upgrade",
			Kind:     "Node", Name: n.Name,
			Title:  fmt.Sprintf("Pool %s is rolling to %s %s", pool, ch.field, ch.val),
			Detail: fmt.Sprintf("first seen on node %s · previously %s", n.Name, ch.was),
			Hint:   kubectl(c.name, "", "get nodes -o wide"),
		})
	}
}

func (c *cluster) onNodeAdd(n *corev1.Node) {
	c.checkNodeInfo(n, false)
	pool := poolOf(n)
	c.store.Record(alert.Alert{
		Key: "node-joined/" + pool, Source: "node-churn", Cluster: c.name, Severity: alert.Info,
		Category: "node", Kind: "Node", Name: n.Name,
		Title:  "Node(s) joined pool " + pool,
		Detail: fmt.Sprintf("latest %s · kubelet %s", n.Name, n.Status.NodeInfo.KubeletVersion),
		Hint:   kubectl(c.name, "", "get nodes -o wide"),
	})
}

func (c *cluster) onNodeDelete(n *corev1.Node) {
	pool := poolOf(n)
	c.store.Record(alert.Alert{
		Key: "node-removed/" + pool, Source: "node-churn", Cluster: c.name, Severity: alert.Info,
		Category: "node", Kind: "Node", Name: n.Name,
		Title:  "Node(s) removed from pool " + pool,
		Detail: "latest " + n.Name,
		Hint:   kubectl(c.name, "", "get nodes -o wide"),
	})
}
