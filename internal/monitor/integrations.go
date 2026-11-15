package monitor

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	"github.com/dlebee/k8s-buddy/internal/alert"
)

const (
	helmPendingFor      = 15 * time.Minute
	certNotReadyFor     = 10 * time.Minute
	certRenewalGrace    = time.Hour
	certUrgentWindow    = 72 * time.Hour
	issuerNotReadyFor   = 5 * time.Minute
	ingressNoAddressFor = 5 * time.Minute
)

// ---------- Helm ----------

// Helm v3 stores each release revision as a Secret labelled owner=helm. We only watch the
// metadata of those secrets (labels carry name/status/version), never their payload.
func stripHelmMeta(obj any) (any, error) {
	if m, ok := obj.(*metav1.PartialObjectMetadata); ok {
		m.ManagedFields = nil
		m.Annotations = nil
	}
	return obj, nil
}

type helmRelease struct {
	namespace, name, status string
	revision                int
	updated                 time.Time
	chart                   string        // from helm.sh/chart label (fallback only)
	workloads               []helmOwnedRes // populated when derived from labels, not secrets
}

type helmOwnedRes struct{ kind, namespace, name string }

// helmReleases returns the latest revision of each release. It prefers Helm's
// own storage secrets (via c.helm); when those aren't readable (RBAC), it
// falls back to scanning workloads for the standard Helm metadata labels.
// The fallback can't reconstruct history, values, or revision numbers.
func (c *cluster) helmReleases() []helmRelease {
	latest := map[string]helmRelease{}
	if c.helm != nil {
		objs, _ := c.helm.List(labels.Everything())
		for _, o := range objs {
			m, ok := o.(*metav1.PartialObjectMetadata)
			if !ok {
				continue
			}
			l := m.Labels
			rev, _ := strconv.Atoi(l["version"])
			r := helmRelease{namespace: m.Namespace, name: l["name"], status: l["status"], revision: rev, updated: m.CreationTimestamp.Time}
			if ts, err := strconv.ParseInt(l["modifiedAt"], 10, 64); err == nil {
				r.updated = time.Unix(ts, 0)
			}
			key := r.namespace + "/" + r.name
			if cur, ok := latest[key]; !ok || r.revision > cur.revision {
				latest[key] = r
			}
		}
	}
	for key, r := range c.helmReleasesFromWorkloads() {
		if _, ok := latest[key]; !ok {
			latest[key] = r
		}
	}
	out := make([]helmRelease, 0, len(latest))
	for _, r := range latest {
		out = append(out, r)
	}
	return out
}

// helmWorkloadFallbackReady reports whether at least one workload informer is
// synced so a label-based Helm discovery can be attempted without the storage
// secret.
func (c *cluster) helmWorkloadFallbackReady() bool {
	return c.synced("deployments") || c.synced("statefulsets") || c.synced("daemonsets") || c.synced("services") || c.synced("ingresses")
}

// helmReleasesFromWorkloads discovers Helm releases by their standard metadata
// labels/annotations on resources the user can list, used as a fallback when
// Helm's own storage secrets aren't readable.
func (c *cluster) helmReleasesFromWorkloads() map[string]helmRelease {
	sel, err := labels.Parse("app.kubernetes.io/managed-by=Helm")
	if err != nil {
		return nil
	}
	out := map[string]helmRelease{}
	add := func(kind string, m metav1.Object) {
		ann := m.GetAnnotations()
		name, ns := ann["meta.helm.sh/release-name"], ann["meta.helm.sh/release-namespace"]
		if name == "" {
			return
		}
		if ns == "" {
			ns = m.GetNamespace()
		}
		key := ns + "/" + name
		r := out[key]
		r.namespace, r.name = ns, name
		if r.status == "" {
			r.status = "unknown"
		}
		if ts := m.GetCreationTimestamp().Time; ts.After(r.updated) {
			r.updated = ts
		}
		if r.chart == "" {
			r.chart = m.GetLabels()["helm.sh/chart"]
		}
		r.workloads = append(r.workloads, helmOwnedRes{kind: kind, namespace: m.GetNamespace(), name: m.GetName()})
		out[key] = r
	}
	if c.deploys != nil {
		if l, _ := c.deploys.List(sel); len(l) > 0 {
			for _, x := range l {
				add("Deployment", x)
			}
		}
	}
	if c.sts != nil {
		if l, _ := c.sts.List(sel); len(l) > 0 {
			for _, x := range l {
				add("StatefulSet", x)
			}
		}
	}
	if c.ds != nil {
		if l, _ := c.ds.List(sel); len(l) > 0 {
			for _, x := range l {
				add("DaemonSet", x)
			}
		}
	}
	if c.services != nil {
		if l, _ := c.services.List(sel); len(l) > 0 {
			for _, x := range l {
				add("Service", x)
			}
		}
	}
	if c.ingresses != nil {
		if l, _ := c.ingresses.List(sel); len(l) > 0 {
			for _, x := range l {
				add("Ingress", x)
			}
		}
	}
	for k, r := range out {
		sort.Slice(r.workloads, func(i, j int) bool {
			if r.workloads[i].kind != r.workloads[j].kind {
				return r.workloads[i].kind < r.workloads[j].kind
			}
			if r.workloads[i].namespace != r.workloads[j].namespace {
				return r.workloads[i].namespace < r.workloads[j].namespace
			}
			return r.workloads[i].name < r.workloads[j].name
		})
		out[k] = r
	}
	return out
}

func helmHealth(r helmRelease) Health {
	switch {
	case r.status == "deployed":
		return HealthOK
	case r.status == "failed":
		return HealthBad
	case strings.HasPrefix(r.status, "pending") || r.status == "uninstalling":
		if time.Since(r.updated) > helmPendingFor {
			return HealthBad
		}
		return HealthWarn
	default:
		return HealthNeutral
	}
}

func listHelm(c *cluster) []Resource {
	rels := c.helmReleases()
	out := make([]Resource, 0, len(rels))
	for _, r := range rels {
		rev := strconv.Itoa(r.revision)
		if r.revision == 0 && len(r.workloads) > 0 {
			rev = "?"
		}
		out = append(out, Resource{Namespace: r.namespace, Name: r.name, Status: r.status, Health: helmHealth(r), Created: r.updated,
			Cols: []string{rev, r.updated.Local().Format("2006-01-02 15:04")}})
	}
	return out
}

func helmCmd(cluster, ns, args string) string {
	return "helm --kube-context " + shellQuote(cluster) + " -n " + shellQuote(ns) + " " + args
}

func (m *Manager) helmDetail(clusterName, ns, name string) (ResourceDetail, error) {
	c, err := m.cluster(clusterName)
	if err != nil {
		return ResourceDetail{}, err
	}
	for _, r := range c.helmReleases() {
		if r.namespace != ns || r.name != name {
			continue
		}
		cmds := []string{
			helmCmd(clusterName, ns, "status "+name),
			helmCmd(clusterName, ns, "history "+name),
			helmCmd(clusterName, ns, "get values "+name),
		}
		if len(r.workloads) == 0 {
			// Discovered via secret: full revision info available.
			y := fmt.Sprintf("release: %s\nnamespace: %s\nrevision: %d\nstatus: %s\nupdated: %s\nstorage: secret/sh.helm.release.v1.%s.v%d\n",
				r.name, r.namespace, r.revision, r.status, r.updated.Format(time.RFC3339), r.name, r.revision)
			cmds = append(cmds, helmCmd(clusterName, ns, fmt.Sprintf("rollback %s %d", name, max(r.revision-1, 1))))
			return ResourceDetail{Cluster: clusterName, Kind: "helm", Namespace: ns, Name: name, YAML: y, Events: []Resource{}, Commands: cmds}, nil
		}
		// Fallback: no secret access. Report what the labels tell us.
		var b strings.Builder
		fmt.Fprintf(&b, "release: %s\nnamespace: %s\nstatus: %s  # storage secret not readable; status, revision, values and history unavailable\n",
			r.name, r.namespace, r.status)
		if r.chart != "" {
			fmt.Fprintf(&b, "chart: %s\n", r.chart)
		}
		if !r.updated.IsZero() {
			fmt.Fprintf(&b, "latestResourceUpdate: %s\n", r.updated.Format(time.RFC3339))
		}
		fmt.Fprintf(&b, "resources:\n")
		for _, w := range r.workloads {
			fmt.Fprintf(&b, "  - %s %s/%s\n", w.kind, w.namespace, w.name)
		}
		return ResourceDetail{Cluster: clusterName, Kind: "helm", Namespace: ns, Name: name, YAML: b.String(), Events: []Resource{}, Commands: cmds}, nil
	}
	return ResourceDetail{}, fmt.Errorf("release %s/%s not found", ns, name)
}

func (c *cluster) detectHelm() []alert.Alert {
	var out []alert.Alert
	for _, r := range c.helmReleases() {
		hint := strings.Join([]string{helmCmd(c.name, r.namespace, "status "+r.name), helmCmd(c.name, r.namespace, "history "+r.name)}, "\n")
		base := alert.Alert{Category: "helm", Kind: "HelmRelease", Namespace: r.namespace, Name: r.name, Hint: hint}
		switch {
		case r.status == "failed":
			base.Key, base.Severity = "helm-failed/"+r.namespace+"/"+r.name, alert.Warning
			base.Title = "Helm release failed · " + r.name
			base.Detail = fmt.Sprintf("revision %d · %s ago", r.revision, ago(r.updated))
		case (strings.HasPrefix(r.status, "pending") || r.status == "uninstalling") && time.Since(r.updated) > helmPendingFor:
			base.Key, base.Severity = "helm-stuck/"+r.namespace+"/"+r.name, alert.Warning
			base.Title = fmt.Sprintf("Helm release stuck in %s · %s", r.status, r.name)
			base.Detail = fmt.Sprintf("revision %d for %s — new upgrades will fail with \"another operation is in progress\"", r.revision, ago(r.updated))
		default:
			continue
		}
		out = append(out, base)
	}
	return out
}

// ---------- CRD-backed watchers (cert-manager) ----------

type crdWatch struct{ id, group, resource string }

var crdWatches = []crdWatch{
	{"cm-certificates", "cert-manager.io", "certificates"},
	{"cm-issuers", "cert-manager.io", "issuers"},
	{"cm-clusterissuers", "cert-manager.io", "clusterissuers"},
}

// ensureCRDWatchers starts informers for integrations whose CRDs exist on the cluster.
func (c *cluster) ensureCRDWatchers() {
	started := false
	for _, w := range crdWatches {
		c.mu.Lock()
		exists := c.informers[w.id] != nil
		c.mu.Unlock()
		if exists {
			continue
		}
		res, ok := c.findResource(w.group, w.resource)
		if !ok {
			continue
		}
		c.mu.Lock()
		if c.dynf == nil {
			dyn, err := dynamic.NewForConfig(c.cfg)
			if err != nil {
				c.mu.Unlock()
				return
			}
			c.dynf = dynamicinformer.NewDynamicSharedInformerFactory(dyn, 0)
		}
		gi := c.dynf.ForResource(res.gvr())
		inf := gi.Informer()
		_ = inf.SetTransform(stripManagedFields)
		_ = inf.SetWatchErrorHandler(func(_ *cache.Reflector, err error) { c.setResourceErr(w.resource, err) })
		c.informers[w.id] = inf
		if c.dynListers == nil {
			c.dynListers = map[string]cache.GenericLister{}
		}
		c.dynListers[w.id] = gi.Lister()
		c.mu.Unlock()
		started = true
	}
	if started {
		c.dynf.Start(c.runCtx.Done())
	}
}

func (c *cluster) unstructuredList(id string) []map[string]any {
	c.mu.Lock()
	l := c.dynListers[id]
	c.mu.Unlock()
	if l == nil {
		return nil
	}
	objs, _ := l.List(labels.Everything())
	out := make([]map[string]any, 0, len(objs))
	for _, o := range objs {
		if u, ok := o.(*unstructured.Unstructured); ok {
			out = append(out, u.UnstructuredContent())
		}
	}
	return out
}

func readyCondition(obj map[string]any) (status, reason, message string, since time.Time) {
	conds, _ := dig(obj, "status", "conditions").([]any)
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || str(m["type"]) != "Ready" {
			continue
		}
		since, _ = time.Parse(time.RFC3339, str(m["lastTransitionTime"]))
		return str(m["status"]), str(m["reason"]), str(m["message"]), since
	}
	return "", "", "", time.Time{}
}

func parseTime(v any) time.Time {
	t, _ := time.Parse(time.RFC3339, str(v))
	return t
}

func until(t time.Time) string {
	d := time.Until(t)
	switch {
	case d < 0:
		return "already"
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("in %dh", int(d.Hours()))
	default:
		return fmt.Sprintf("in %dd", int(d.Hours()/24))
	}
}

// detectCertManager flags certificates that expired, are overdue for renewal, or are stuck not
// ready, plus issuers that are not ready. A short-lived cert that renews on schedule is fine.
func (c *cluster) detectCertManager() []alert.Alert {
	now := time.Now()
	var out []alert.Alert
	for _, obj := range c.unstructuredList("cm-certificates") {
		ns, name := str(dig(obj, "metadata", "namespace")), str(dig(obj, "metadata", "name"))
		notAfter, renewal := parseTime(dig(obj, "status", "notAfter")), parseTime(dig(obj, "status", "renewalTime"))
		status, reason, msg, since := readyCondition(obj)
		hint := strings.Join([]string{
			kubectl(c.name, ns, "describe certificate.cert-manager.io/"+name),
			kubectl(c.name, ns, "get certificaterequests,orders.acme.cert-manager.io,challenges.acme.cert-manager.io"),
		}, "\n")
		base := alert.Alert{Category: "certificate", Kind: "Certificate", Namespace: ns, Name: name, Hint: hint}
		why := joinNonEmpty(" · ", reason, truncate(msg, 200))
		switch {
		case !notAfter.IsZero() && now.After(notAfter):
			a := base
			a.Key, a.Severity = "cert-expired/"+ns+"/"+name, alert.Critical
			a.Title = "Certificate expired · " + name
			a.Detail = joinNonEmpty(" · ", "expired "+ago(notAfter)+" ago", why)
			out = append(out, a)
		case !renewal.IsZero() && now.After(renewal.Add(certRenewalGrace)):
			a := base
			a.Key, a.Severity = "cert-renewal-overdue/"+ns+"/"+name, alert.Warning
			if !notAfter.IsZero() && notAfter.Sub(now) < certUrgentWindow {
				a.Severity = alert.Critical
			}
			a.Title = "Certificate not renewing · " + name
			a.Detail = joinNonEmpty(" · ", "renewal was due "+ago(renewal)+" ago", "expires "+until(notAfter), why)
			out = append(out, a)
		}
		if status == "False" && now.Sub(since) > certNotReadyFor {
			a := base
			a.Key, a.Severity = "cert-not-ready/"+ns+"/"+name, alert.Warning
			a.Title = "Certificate not ready · " + name
			a.Detail = joinNonEmpty(" · ", why, "for "+ago(since))
			out = append(out, a)
		}
	}
	for _, id := range []string{"cm-issuers", "cm-clusterissuers"} {
		for _, obj := range c.unstructuredList(id) {
			ns, name := str(dig(obj, "metadata", "namespace")), str(dig(obj, "metadata", "name"))
			status, reason, msg, since := readyCondition(obj)
			if status != "False" || time.Since(since) < issuerNotReadyFor {
				continue
			}
			kind, res := "Issuer", "issuer.cert-manager.io/"
			if id == "cm-clusterissuers" {
				kind, res = "ClusterIssuer", "clusterissuer.cert-manager.io/"
			}
			out = append(out, alert.Alert{
				Key: "issuer-not-ready/" + ns + "/" + name, Severity: alert.Warning, Category: "certificate", Kind: kind, Namespace: ns, Name: name,
				Title:  kind + " not ready · " + name,
				Detail: joinNonEmpty(" · ", reason, truncate(msg, 200)),
				Hint:   kubectl(c.name, ns, "describe "+res+name),
			})
		}
	}
	return out
}

// ---------- Ingress ----------

func ingressClassOf(annotations map[string]string, className *string) string {
	if className != nil && *className != "" {
		return *className
	}
	return annotations["kubernetes.io/ingress.class"]
}

func (c *cluster) detectIngresses() []alert.Alert {
	ings, _ := c.ingresses.List(labels.Everything())
	ci := c.classInfo()
	merge := nginxMergeIssues(ings, ci)
	var out []alert.Alert
	for _, ing := range ings {
		if len(ing.Status.LoadBalancer.Ingress) > 0 || ing.DeletionTimestamp != nil || time.Since(ing.CreationTimestamp.Time) < ingressNoAddressFor {
			continue
		}
		class := ingressClassOf(ing.Annotations, ing.Spec.IngressClassName)
		var detail string
		switch ctrl := ci.controllerFor(class); {
		case class == "" && ci.def == "":
			detail = "no ingress class set and the cluster has no default class"
		case ctrl == "" && len(ci.controllers) > 0:
			detail = fmt.Sprintf("ingress class %q doesn't exist", class)
		case ctrl != "":
			detail = fmt.Sprintf("class %q (%s) hasn't published an address — the controller likely rejected it; check its logs", firstNonEmpty(class, ci.def), ctrl)
		default:
			detail = fmt.Sprintf("class %q — no controller has picked it up", class)
		}
		title := "Ingress has no address · ingress/" + ing.Name
		if mi, ok := merge[ingressKey(ing)]; ok {
			title, detail = "Ingress ignored by NGINX · ingress/"+ing.Name, mi.Message
		}
		out = append(out, alert.Alert{
			Key: "ingress-no-address/" + ing.Namespace + "/" + ing.Name, Severity: alert.Warning, Category: "network",
			Kind: "Ingress", Namespace: ing.Namespace, Name: ing.Name,
			Title:  title,
			Detail: detail + " · " + ago(ing.CreationTimestamp.Time) + " old",
			Hint:   kubectl(c.name, ing.Namespace, "describe ingress "+ing.Name) + "\n" + kubectl(c.name, "", "get ingressclass"),
		})
	}
	return out
}

// ---------- ingress controllers ----------

const (
	nginxIncController  = "nginx.org/ingress-controller"
	communityController = "k8s.io/ingress-nginx"
)

type classInfo struct {
	controllers map[string]string
	def         string
}

func classesFrom(list []*networkingv1.IngressClass) classInfo {
	ci := classInfo{controllers: map[string]string{}}
	for _, ic := range list {
		ci.controllers[ic.Name] = ic.Spec.Controller
		if ic.Annotations[networkingv1.AnnotationIsDefaultIngressClass] == "true" {
			ci.def = ic.Name
		}
	}
	return ci
}

func (ci classInfo) controllerFor(class string) string {
	if class == "" {
		class = ci.def
	}
	return ci.controllers[class]
}

func (c *cluster) classInfo() classInfo {
	if c.ingressClasses == nil || !c.synced("ingressclasses") {
		return classInfo{}
	}
	list, _ := c.ingressClasses.List(labels.Everything())
	return classesFrom(list)
}

// Annotation prefixes each NGINX flavour understands; the other one silently ignores them.
var annotationPrefix = map[string]string{
	nginxIncController:  "nginx.org/",
	communityController: "nginx.ingress.kubernetes.io/",
}

var securityAnnotations = []string{"whitelist-source-range", "allowlist-source-range", "denylist-source-range", "auth-", "enable-modsecurity", "basic-auth", "jwt"}

// foreignAnnotations returns annotations meant for the other NGINX controller than the one serving ing.
func foreignAnnotations(ing *networkingv1.Ingress, controller string) (keys []string, security bool) {
	for other, prefix := range annotationPrefix {
		if other == controller || annotationPrefix[controller] == "" {
			continue
		}
		for k := range ing.Annotations {
			if name, ok := strings.CutPrefix(k, prefix); ok {
				keys = append(keys, name)
				for _, s := range securityAnnotations {
					if strings.HasPrefix(name, s) {
						security = true
					}
				}
			}
		}
	}
	slices.Sort(keys)
	return keys, security
}

// ---------- NGINX (F5 nginx.org) mergeable ingresses ----------

const mergeableAnnotation = "nginx.org/mergeable-ingress-type"

type mergeIssue struct {
	Severity alert.Severity
	Message  string
}

func ingressKey(ing *networkingv1.Ingress) string { return ing.Namespace + "/" + ing.Name }

func ingressPaths(ing *networkingv1.Ingress) int {
	n := 0
	for _, r := range ing.Spec.Rules {
		if r.HTTP != nil {
			n += len(r.HTTP.Paths)
		}
	}
	return n
}

// nginxMergeIssues finds mergeable ingresses NGINX will ignore: minions without a master, masters
// without minions, masters with paths, and hosts claimed by more than one master/plain ingress
// (NGINX keeps the oldest). Results are keyed by namespace/name.
func nginxMergeIssues(ings []*networkingv1.Ingress, ci classInfo) map[string]mergeIssue {
	type member struct {
		ing *networkingv1.Ingress
		typ string
	}
	groups := map[string][]member{}
	var hosts []string
	for _, ing := range ings {
		if ing.DeletionTimestamp != nil {
			continue
		}
		class := ingressClassOf(ing.Annotations, ing.Spec.IngressClassName)
		seen := map[string]bool{}
		for _, r := range ing.Spec.Rules {
			if r.Host == "" || seen[r.Host] {
				continue
			}
			seen[r.Host] = true
			g := class + "|" + r.Host
			if groups[g] == nil {
				hosts = append(hosts, g)
			}
			groups[g] = append(groups[g], member{ing, ing.Annotations[mergeableAnnotation]})
		}
	}
	out := map[string]mergeIssue{}
	set := func(ing *networkingv1.Ingress, sev alert.Severity, msg string) {
		if cur, ok := out[ingressKey(ing)]; !ok || sev.Rank() > cur.Severity.Rank() {
			out[ingressKey(ing)] = mergeIssue{sev, msg}
		}
	}
	slices.Sort(hosts)
	for _, g := range hosts {
		host := g[strings.Index(g, "|")+1:]
		var contenders, minions []member
		mergeable := false
		for _, m := range groups[g] {
			switch m.typ {
			case "master":
				mergeable = true
				contenders = append(contenders, m)
				if ingressPaths(m.ing) > 0 {
					set(m.ing, alert.Warning, "master ingresses can't define paths — NGINX rejects this master (move the paths into minions)")
				}
			case "minion":
				mergeable = true
				minions = append(minions, m)
			default:
				contenders = append(contenders, m)
			}
		}
		class := g[:strings.Index(g, "|")]
		if !mergeable {
			// NGINX Inc doesn't merge separate plain ingresses for one host: it serves one and
			// rejects the others. Trust the controller's verdict: only the ones without an address.
			if ci.controllerFor(class) == nginxIncController && len(contenders) > 1 {
				var served *networkingv1.Ingress
				for _, m := range contenders {
					if len(m.ing.Status.LoadBalancer.Ingress) > 0 && served == nil {
						served = m.ing
					}
				}
				if served != nil {
					for _, m := range contenders {
						if len(m.ing.Status.LoadBalancer.Ingress) == 0 {
							msg := fmt.Sprintf("%s is already served by %s and NGINX (nginx.org) doesn't merge separate ingresses for one host, so this one is ignored", host, ingressKey(served))
							if d := lossDetail(m.ing, served, host); d != "" {
								msg += ": " + d
							}
							set(m.ing, alert.Warning, msg+". Put the paths in one ingress, or use master/minions.")
						}
					}
				}
			}
			continue
		}
		slices.SortStableFunc(contenders, func(a, b member) int {
			if c := a.ing.CreationTimestamp.Compare(b.ing.CreationTimestamp.Time); c != 0 {
				return c
			}
			return strings.Compare(ingressKey(a.ing), ingressKey(b.ing))
		})
		if len(contenders) == 0 {
			for _, m := range minions {
				set(m.ing, alert.Warning, fmt.Sprintf("minion for %s has no master — NGINX ignores minions without one", host))
			}
			continue
		}
		winner := contenders[0]
		for _, l := range contenders[1:] {
			msg := fmt.Sprintf("%s is also claimed by %s (older) — NGINX keeps the oldest, so this one is ignored", host, ingressKey(winner.ing))
			if d := lossDetail(l.ing, winner.ing, host); d != "" {
				msg += ": " + d
			}
			set(l.ing, alert.Warning, msg)
		}
		switch {
		case winner.typ == "master" && len(minions) == 0:
			set(winner.ing, alert.Notice, fmt.Sprintf("master for %s has no minions, so it serves no paths", host))
		case winner.typ != "master":
			for _, m := range minions {
				set(m.ing, alert.Warning, fmt.Sprintf("%s is served by plain ingress %s, not a master — this minion is ignored", host, ingressKey(winner.ing)))
			}
		}
		for _, m := range minions {
			if len(m.ing.Spec.TLS) > 0 {
				set(m.ing, alert.Notice, "TLS on a minion is ignored — configure TLS on the master")
			}
		}
	}
	return out
}

// ---------- ingress routing map ----------

type RoutePath struct {
	Path    string    `json:"path"`
	Service string    `json:"service"`
	Port    string    `json:"port,omitempty"`
	FallsTo *RouteHop `json:"fallsTo,omitempty"`
}

type RouteHop struct {
	Ingress string `json:"ingress"`
	Path    string `json:"path"`
	Service string `json:"service"`
}

type RouteIngress struct {
	Namespace string      `json:"namespace"`
	Name      string      `json:"name"`
	Type      string      `json:"type"`
	Served    bool        `json:"served"`
	Address   string      `json:"address,omitempty"`
	Reason    string      `json:"reason,omitempty"`
	TLS       bool        `json:"tls"`
	Ignored   []string    `json:"ignoredAnnotations,omitempty"`
	Paths     []RoutePath `json:"paths"`
	Created   time.Time   `json:"created"`
}

type RouteHost struct {
	Host       string         `json:"host"`
	Class      string         `json:"class"`
	Controller string         `json:"controller,omitempty"`
	Ingresses  []RouteIngress `json:"ingresses"`
}

func pathsOf(ing *networkingv1.Ingress, host string) []RoutePath {
	var out []RoutePath
	for _, r := range ing.Spec.Rules {
		if r.Host != host || r.HTTP == nil {
			continue
		}
		for _, p := range r.HTTP.Paths {
			rp := RoutePath{Path: firstNonEmpty(p.Path, "/")}
			if s := p.Backend.Service; s != nil {
				rp.Service, rp.Port = s.Name, portString(s.Port)
			}
			out = append(out, rp)
		}
	}
	return out
}

// fallthroughFor finds the served route NGINX will actually use for path: the longest matching prefix.
func fallthroughFor(path string, served []RouteIngress) *RouteHop {
	var best *RouteHop
	bestLen := -1
	for _, ri := range served {
		for _, p := range ri.Paths {
			prefix := strings.TrimSuffix(p.Path, "/")
			if (path == p.Path || strings.HasPrefix(path, prefix+"/") || p.Path == "/") && len(p.Path) > bestLen {
				bestLen = len(p.Path)
				best = &RouteHop{Ingress: ri.Namespace + "/" + ri.Name, Path: p.Path, Service: p.Service}
			}
		}
	}
	return best
}

func buildRouteHosts(ings []*networkingv1.Ingress, target *networkingv1.Ingress, ci classInfo) []RouteHost {
	merge := nginxMergeIssues(ings, ci)
	targetClass := ingressClassOf(target.Annotations, target.Spec.IngressClassName)
	var out []RouteHost
	seen := map[string]bool{}
	for _, rule := range target.Spec.Rules {
		host := rule.Host
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		rh := RouteHost{Host: host, Class: firstNonEmpty(targetClass, ci.def), Controller: ci.controllerFor(targetClass)}
		for _, ing := range ings {
			if ingressClassOf(ing.Annotations, ing.Spec.IngressClassName) != targetClass || ing.DeletionTimestamp != nil {
				continue
			}
			if !slices.ContainsFunc(ing.Spec.Rules, func(r networkingv1.IngressRule) bool { return r.Host == host }) {
				continue
			}
			ri := RouteIngress{Namespace: ing.Namespace, Name: ing.Name, Type: firstNonEmpty(ing.Annotations[mergeableAnnotation], "plain"),
				TLS: len(ing.Spec.TLS) > 0, Paths: pathsOf(ing, host), Created: ing.CreationTimestamp.Time}
			if lb := ing.Status.LoadBalancer.Ingress; len(lb) > 0 {
				ri.Served, ri.Address = true, firstNonEmpty(lb[0].IP, lb[0].Hostname)
			}
			if mi, ok := merge[ingressKey(ing)]; ok {
				ri.Reason = mi.Message
			}
			ri.Ignored, _ = foreignAnnotations(ing, rh.Controller)
			if ri.Paths == nil {
				ri.Paths = []RoutePath{}
			}
			rh.Ingresses = append(rh.Ingresses, ri)
		}
		var served []RouteIngress
		for _, ri := range rh.Ingresses {
			if ri.Served && ri.Reason == "" {
				served = append(served, ri)
			}
		}
		for i := range rh.Ingresses {
			ri := &rh.Ingresses[i]
			if ri.Served && ri.Reason == "" {
				continue
			}
			for j := range ri.Paths {
				ri.Paths[j].FallsTo = fallthroughFor(ri.Paths[j].Path, served)
			}
		}
		slices.SortStableFunc(rh.Ingresses, func(a, b RouteIngress) int {
			if a.Served != b.Served {
				if a.Served {
					return -1
				}
				return 1
			}
			return a.Created.Compare(b.Created)
		})
		out = append(out, rh)
	}
	return out
}

var ignorableAnnotation = []string{"meta.helm.sh/", "cert-manager.io/", "kubectl.kubernetes.io/", "field.cattle.io/"}

// lossDetail spells out what is lost when loser is ignored in favour of winner: where its paths
// actually land, and which of its own annotations stop applying.
func lossDetail(loser, winner *networkingv1.Ingress, host string) string {
	won := []RouteIngress{{Namespace: winner.Namespace, Name: winner.Name, Paths: pathsOf(winner, host)}}
	var parts []string
	for _, p := range pathsOf(loser, host) {
		if hop := fallthroughFor(p.Path, won); hop != nil {
			parts = append(parts, fmt.Sprintf("%s is answered by %s's %s route (service %s) instead", p.Path, winner.Name, hop.Path, hop.Service))
		} else {
			parts = append(parts, p.Path+" isn't served at all (404)")
		}
	}
	var lost []string
	for k, v := range loser.Annotations {
		if k == mergeableAnnotation || slices.ContainsFunc(ignorableAnnotation, func(p string) bool { return strings.HasPrefix(k, p) }) || winner.Annotations[k] == v {
			continue
		}
		lost = append(lost, k[strings.LastIndex(k, "/")+1:])
	}
	slices.Sort(lost)
	if len(lost) > 0 {
		parts = append(parts, "its settings don't apply ("+strings.Join(lost, ", ")+")")
	}
	return strings.Join(parts, "; ")
}

// IngressRoutes shows every ingress sharing a host with the given one and where each path really goes.
func (m *Manager) IngressRoutes(clusterName, ns, name string) ([]RouteHost, error) {
	c, err := m.cluster(clusterName)
	if err != nil {
		return nil, err
	}
	if !c.synced("ingresses") {
		return nil, fmt.Errorf("ingresses are still loading on %s", clusterName)
	}
	target, err := c.ingresses.Ingresses(ns).Get(name)
	if err != nil {
		return nil, err
	}
	ings, _ := c.ingresses.List(labels.Everything())
	return buildRouteHosts(ings, target, c.classInfo()), nil
}
