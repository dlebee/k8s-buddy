package monitor

import (
	"fmt"
	"slices"
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
}

func (c *cluster) helmReleases() []helmRelease {
	objs, _ := c.helm.List(labels.Everything())
	latest := map[string]helmRelease{}
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
	out := make([]helmRelease, 0, len(latest))
	for _, r := range latest {
		out = append(out, r)
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
		out = append(out, Resource{Namespace: r.namespace, Name: r.name, Status: r.status, Health: helmHealth(r), Created: r.updated,
			Cols: []string{strconv.Itoa(r.revision), r.updated.Local().Format("2006-01-02 15:04")}})
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
		if r.namespace == ns && r.name == name {
			y := fmt.Sprintf("release: %s\nnamespace: %s\nrevision: %d\nstatus: %s\nupdated: %s\nstorage: secret/sh.helm.release.v1.%s.v%d\n",
				r.name, r.namespace, r.revision, r.status, r.updated.Format(time.RFC3339), r.name, r.revision)
			return ResourceDetail{Cluster: clusterName, Kind: "helm", Namespace: ns, Name: name, YAML: y, Events: []Resource{}, Commands: []string{
				helmCmd(clusterName, ns, "status "+name),
				helmCmd(clusterName, ns, "history "+name),
				helmCmd(clusterName, ns, "get values "+name),
				helmCmd(clusterName, ns, fmt.Sprintf("rollback %s %d", name, max(r.revision-1, 1))),
			}}, nil
		}
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
	merge := nginxMergeIssues(ings)
	var out []alert.Alert
	for _, ing := range ings {
		if len(ing.Status.LoadBalancer.Ingress) > 0 || ing.DeletionTimestamp != nil || time.Since(ing.CreationTimestamp.Time) < ingressNoAddressFor {
			continue
		}
		class := ingressClassOf(ing.Annotations, ing.Spec.IngressClassName)
		detail := "no controller has picked it up"
		if class != "" {
			detail = fmt.Sprintf("class %q — is that controller running?", class)
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
func nginxMergeIssues(ings []*networkingv1.Ingress) map[string]mergeIssue {
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
		if !mergeable {
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
			if l.typ == "master" {
				msg += "; its server-level config (TLS, snippets, annotations) is not applied"
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
