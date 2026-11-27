package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/kubectl/pkg/describe"
	"sigs.k8s.io/yaml"
)

// APIResource is a listable resource type found through discovery.
type APIResource struct {
	ID         string   `json:"id"`
	Group      string   `json:"group"`
	Version    string   `json:"version"`
	Resource   string   `json:"resource"`
	Kind       string   `json:"kind"`
	Namespaced bool     `json:"namespaced"`
	Clusters   []string `json:"clusters,omitempty"`
}

func (r APIResource) gvr() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: r.Group, Version: r.Version, Resource: r.Resource}
}

func (r APIResource) path(namespace, name string) string {
	p := "/apis/" + r.Group + "/" + r.Version
	if r.Group == "" {
		p = "/api/" + r.Version
	}
	if namespace != "" && r.Namespaced {
		p += "/namespaces/" + namespace
	}
	p += "/" + r.Resource
	if name != "" {
		p += "/" + name
	}
	return p
}

// objectPath is the API path of exactly one object. The REST client cleans "." and ".." out of
// absolute paths, so an unchecked name could walk to another resource ("../../secrets"), and an
// empty one would fetch the whole collection — a SecretList is not redacted object by object.
func (r APIResource) objectPath(namespace, name string) (string, error) {
	if !validPathName(name) {
		return "", fmt.Errorf("invalid name %q", name)
	}
	if r.Namespaced {
		if !validObjectName(namespace) {
			return "", fmt.Errorf("invalid namespace %q", namespace)
		}
	} else {
		namespace = ""
	}
	return r.path(namespace, name), nil
}

// singleObject refuses list responses: the detail views only ever show one object, which always
// has a name (lists don't).
func singleObject(obj map[string]any) error {
	if k := str(obj["kind"]); strings.HasSuffix(k, "List") || str(dig(obj, "metadata", "name")) == "" {
		return fmt.Errorf("expected one object, got %s", firstNonEmpty(k, "something else"))
	}
	return nil
}

type genericKind struct {
	label string
	res   APIResource
}

// Built-in kinds rendered through the API server's own table output (same columns as kubectl get).
var genericBuiltins = map[string]genericKind{
	"ingresses":       {"Ingresses", APIResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses", Kind: "Ingress", Namespaced: true}},
	"jobs":            {"Jobs", APIResource{Group: "batch", Version: "v1", Resource: "jobs", Kind: "Job", Namespaced: true}},
	"cronjobs":        {"CronJobs", APIResource{Group: "batch", Version: "v1", Resource: "cronjobs", Kind: "CronJob", Namespaced: true}},
	"hpas":            {"Autoscalers", APIResource{Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers", Kind: "HorizontalPodAutoscaler", Namespaced: true}},
	"pdbs":            {"Disruption budgets", APIResource{Group: "policy", Version: "v1", Resource: "poddisruptionbudgets", Kind: "PodDisruptionBudget", Namespaced: true}},
	"networkpolicies": {"Network policies", APIResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies", Kind: "NetworkPolicy", Namespaced: true}},
	"pvs":             {"Persistent volumes", APIResource{Version: "v1", Resource: "persistentvolumes", Kind: "PersistentVolume"}},
	"storageclasses":  {"Storage classes", APIResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses", Kind: "StorageClass"}},
	"namespaces":      {"Namespaces", APIResource{Version: "v1", Resource: "namespaces", Kind: "Namespace"}},
	// Secrets are listed as a table with metadata only (no values ever leave the API server) and
	// fetched one at a time, redacted, when someone opens one.
	"secrets": {"Secrets", APIResource{Version: "v1", Resource: "secrets", Kind: "Secret", Namespaced: true}},
}

// includeObjectFor is what a table listing asks the API server to embed in each row: the full object
// (for health from status), or only metadata for Secrets so their values are never downloaded.
func includeObjectFor(r APIResource) string {
	if r.Group == "" && r.Resource == "secrets" {
		return "Metadata"
	}
	return "Object"
}

// genericFor resolves built-in generic kinds and custom resources ("cr:<group>/<resource>").
func genericFor(kind string) (genericKind, bool) {
	if g, ok := genericBuiltins[kind]; ok {
		return g, true
	}
	if id, ok := strings.CutPrefix(kind, "cr:"); ok {
		group, resource, ok := strings.Cut(id, "/")
		// The core group (Secrets, ConfigMaps, ...) has dedicated kinds with redaction; never reach it
		// through the raw generic path.
		if ok && resource != "" && group != "" {
			return genericKind{label: resource, res: APIResource{Group: group, Resource: resource}}, true
		}
	}
	return genericKind{}, false
}

var builtinGroups = map[string]bool{"": true, "apps": true, "batch": true, "policy": true, "autoscaling": true, "extensions": true}

func isCustomGroup(g string) bool {
	return !builtinGroups[g] && !strings.HasSuffix(g, ".k8s.io") || strings.HasPrefix(g, "gateway.networking.")
}

type discoveryCache struct {
	mu   sync.Mutex
	at   time.Time
	list []APIResource
}

const discoveryTTL = 10 * time.Minute

// apiResources returns the cluster's preferred listable resources, cached.
func (c *cluster) apiResources() []APIResource {
	c.disc.mu.Lock()
	defer c.disc.mu.Unlock()
	if c.disc.list != nil && time.Since(c.disc.at) < discoveryTTL {
		return c.disc.list
	}
	cfg := rest.CopyConfig(c.cfg)
	cfg.Timeout = 20 * time.Second
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return c.disc.list
	}
	lists, err := dc.ServerPreferredResources()
	if len(lists) == 0 && err != nil {
		return c.disc.list
	}
	var out []APIResource
	for _, l := range lists {
		gv, err := schema.ParseGroupVersion(l.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range l.APIResources {
			if strings.Contains(r.Name, "/") || !slices.Contains(r.Verbs, "list") {
				continue
			}
			out = append(out, APIResource{
				ID: gv.Group + "/" + r.Name, Group: gv.Group, Version: gv.Version, Resource: r.Name, Kind: r.Kind, Namespaced: r.Namespaced,
			})
		}
	}
	c.disc.list, c.disc.at = out, time.Now()
	return out
}

func (c *cluster) findResource(group, resource string) (APIResource, bool) {
	for _, r := range c.apiResources() {
		if r.Group == group && r.Resource == resource {
			return r, true
		}
	}
	return APIResource{}, false
}

// CustomResources lists custom resource types across connected clusters.
func (m *Manager) CustomResources() []APIResource {
	byID := map[string]*APIResource{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range m.connected() {
		if !c.synced("pods") {
			continue
		}
		wg.Go(func() {
			for _, r := range c.apiResources() {
				if !isCustomGroup(r.Group) {
					continue
				}
				mu.Lock()
				if cur, ok := byID[r.ID]; ok {
					cur.Clusters = append(cur.Clusters, c.name)
				} else {
					r.Clusters = []string{c.name}
					byID[r.ID] = &r
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	out := make([]APIResource, 0, len(byID))
	for _, r := range byID {
		slices.Sort(r.Clusters)
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b APIResource) int { return strings.Compare(a.Group+"/"+a.Kind, b.Group+"/"+b.Kind) })
	return out
}

type tableCacheEntry struct {
	at      time.Time
	columns []string
	rows    []Resource
	err     error
}

type tableCache struct {
	mu sync.Mutex
	m  map[string]tableCacheEntry
}

// table lists a resource via the server-side Table format, so any type (CRDs included) gets the
// same columns `kubectl get` shows.
func (c *cluster) table(ctx context.Context, r APIResource) ([]string, []Resource, error) {
	key := r.path("", "")
	c.tables.mu.Lock()
	if e, ok := c.tables.m[key]; ok && time.Since(e.at) < 10*time.Second {
		c.tables.mu.Unlock()
		return e.columns, e.rows, e.err
	}
	c.tables.mu.Unlock()

	raw, err := c.cs.CoreV1().RESTClient().Get().AbsPath(key).
		Param("includeObject", includeObjectFor(r)).
		SetHeader("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io,application/json").
		DoRaw(ctx)
	var cols []string
	var rows []Resource
	if err == nil {
		var t metav1.Table
		if err = json.Unmarshal(raw, &t); err == nil {
			cols, rows = tableRows(&t, r)
		}
	}
	c.tables.mu.Lock()
	if c.tables.m == nil {
		c.tables.m = map[string]tableCacheEntry{}
	}
	c.tables.m[key] = tableCacheEntry{at: time.Now(), columns: cols, rows: rows, err: err}
	c.tables.mu.Unlock()
	return cols, rows, err
}

func tableRows(t *metav1.Table, r APIResource) ([]string, []Resource) {
	var idx []int
	var cols []string
	for i, cd := range t.ColumnDefinitions {
		if cd.Priority > 0 || cd.Name == "Name" || cd.Name == "Age" || cd.Name == "Namespace" {
			continue
		}
		idx = append(idx, i)
		cols = append(cols, cd.Name)
	}
	rows := make([]Resource, 0, len(t.Rows))
	for _, row := range t.Rows {
		var obj map[string]any
		_ = json.Unmarshal(row.Object.Raw, &obj)
		md, _ := obj["metadata"].(map[string]any)
		res := Resource{Name: str(md["name"]), Namespace: str(md["namespace"])}
		if ts, err := time.Parse(time.RFC3339, str(md["creationTimestamp"])); err == nil {
			res.Created = ts
		}
		res.Status, res.Health = objectHealth(r, obj)
		for _, i := range idx {
			if i < len(row.Cells) {
				res.Cols = append(res.Cols, cellString(row.Cells[i]))
			}
		}
		rows = append(rows, res)
	}
	return cols, rows
}

func cellString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprint(int64(x))
		}
		return fmt.Sprint(x)
	default:
		return fmt.Sprint(x)
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func dig(obj map[string]any, path ...string) any {
	var cur any = obj
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

// objectHealth derives a status from common conventions: status.conditions, then status.phase.
func objectHealth(r APIResource, obj map[string]any) (string, Health) {
	if r.Kind == "Ingress" || r.Resource == "ingresses" {
		if lb, _ := dig(obj, "status", "loadBalancer", "ingress").([]any); len(lb) > 0 {
			return "Has address", HealthOK
		}
		return "No address", HealthWarn
	}
	switch r.Resource {
	case "persistentvolumes":
		switch phase := str(dig(obj, "status", "phase")); phase {
		case "Bound":
			return phase, HealthOK
		case "Released":
			return phase, HealthWarn
		case "Failed":
			return phase, HealthBad
		default:
			return firstNonEmpty(phase, "–"), HealthNeutral
		}
	case "namespaces":
		if phase := str(dig(obj, "status", "phase")); phase == "Terminating" {
			return phase, HealthWarn
		}
		return "Active", HealthOK
	}
	conds, _ := dig(obj, "status", "conditions").([]any)
	byType := map[string]map[string]any{}
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok {
			byType[str(m["type"])] = m
		}
	}
	if f := byType["Failed"]; f != nil && str(f["status"]) == "True" {
		return firstNonEmpty(str(f["reason"]), "Failed"), HealthBad
	}
	if f := byType["Complete"]; f != nil && str(f["status"]) == "True" {
		return "Complete", HealthNeutral
	}
	if f := byType["DisruptionAllowed"]; f != nil {
		if n, _ := dig(obj, "status", "expectedPods").(float64); n == 0 {
			return "No pods", HealthNeutral
		}
		if str(f["status"]) == "True" {
			return "Disruptions allowed", HealthOK
		}
		return "Blocks disruptions", HealthWarn
	}
	if f := byType["ScalingActive"]; f != nil && str(f["status"]) == "False" {
		return firstNonEmpty(str(f["reason"]), "Not scaling"), HealthWarn
	}
	for _, t := range []string{"Ready", "Available", "Synced", "Healthy", "Established"} {
		cond := byType[t]
		if cond == nil {
			continue
		}
		switch str(cond["status"]) {
		case "True":
			return t, HealthOK
		case "False":
			return firstNonEmpty(str(cond["reason"]), "Not "+strings.ToLower(t)), HealthBad
		default:
			return firstNonEmpty(str(cond["reason"]), "Unknown"), HealthWarn
		}
	}
	if phase := str(dig(obj, "status", "phase")); phase != "" {
		return phase, HealthNeutral
	}
	if r.Resource == "cronjobs" {
		if b, _ := dig(obj, "spec", "suspend").(bool); b {
			return "Suspended", HealthNeutral
		}
		return "Scheduled", HealthOK
	}
	return "–", HealthNeutral
}

func (m *Manager) genericResources(q ResourceQuery, g genericKind) ResourceList {
	out := newResourceList(q.Kind, []string{}, true)
	type result struct {
		c    *cluster
		res  APIResource
		cols []string
		rows []Resource
		err  error
	}
	var mu sync.Mutex
	var results []result
	var wg sync.WaitGroup
	for _, c := range m.connected() {
		if !inScope(q.Clusters, c.name) || !c.synced("pods") {
			continue
		}
		res, ok := c.findResource(g.res.Group, g.res.Resource)
		if !ok {
			continue
		}
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cols, rows, err := c.table(ctx, res)
			mu.Lock()
			results = append(results, result{c, res, cols, rows, err})
			mu.Unlock()
		})
	}
	wg.Wait()
	slices.SortFunc(results, func(a, b result) int { return strings.Compare(a.c.name, b.c.name) })
	var rows []Resource
	for _, r := range results {
		out.Namespaced = r.res.Namespaced
		if r.err != nil {
			reason := "error"
			if apierrors.IsForbidden(r.err) || strings.Contains(strings.ToLower(r.err.Error()), "forbidden") {
				reason = "no access"
			}
			out.Unavailable = append(out.Unavailable, Unavailable{Cluster: r.c.name, Reason: reason, Detail: truncate(r.err.Error(), 300)})
			continue
		}
		if len(out.Columns) == 0 {
			out.Columns = r.cols
		}
		for _, row := range r.rows {
			row.Cluster = r.c.name
			row.Cols = alignColumns(out.Columns, r.cols, row.Cols)
			rows = append(rows, row)
		}
	}
	out.apply(rows, q)
	return out
}

// alignColumns maps values to the reference column order (CRD versions may print different columns).
func alignColumns(ref, cols, vals []string) []string {
	if slices.Equal(ref, cols) {
		return vals
	}
	out := make([]string, len(ref))
	for i, name := range ref {
		if j := slices.Index(cols, name); j >= 0 && j < len(vals) {
			out[i] = vals[j]
		}
	}
	return out
}

func (m *Manager) cluster(name string) (*cluster, error) {
	for _, c := range m.connected() {
		if c.name == name && c.synced("pods") {
			return c, nil
		}
	}
	return nil, fmt.Errorf("cluster %q is not connected", name)
}

func (m *Manager) genericDetail(clusterName, kind string, g genericKind, ns, name string) (ResourceDetail, error) {
	c, err := m.cluster(clusterName)
	if err != nil {
		return ResourceDetail{}, err
	}
	res, ok := c.findResource(g.res.Group, g.res.Resource)
	if !ok {
		return ResourceDetail{}, fmt.Errorf("%s not served by %s", g.res.Resource, clusterName)
	}
	p, err := res.objectPath(ns, name)
	if err != nil {
		return ResourceDetail{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	raw, err := c.cs.CoreV1().RESTClient().Get().AbsPath(p).DoRaw(ctx)
	if err != nil {
		return ResourceDetail{}, err
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ResourceDetail{}, err
	}
	if err := singleObject(obj); err != nil {
		return ResourceDetail{}, err
	}
	redactObject(obj)
	b, err := yaml.Marshal(obj)
	if err != nil {
		return ResourceDetail{}, err
	}
	d := ResourceDetail{Cluster: clusterName, Kind: kind, Namespace: ns, Name: name, YAML: string(b), Events: []Resource{}}
	if c.synced("events") {
		evs, _ := c.events.List(labels.Everything())
		for _, e := range evs {
			if e.InvolvedObject.Kind == res.Kind && e.InvolvedObject.Name == name && e.InvolvedObject.Namespace == ns {
				d.Events = append(d.Events, eventRow(e))
			}
		}
	}
	ref := res.Resource
	if res.Group != "" {
		ref += "." + res.Group
	}
	ref += "/" + name
	d.Commands = []string{kubectl(clusterName, ns, "describe "+ref), kubectl(clusterName, ns, "get "+ref+" -o yaml")}
	if res.Group == "" && res.Resource == "secrets" {
		// redactObject left only the key names; offer to read one, which the console confirms first.
		hidden, _ := obj["stringData"].(map[string]any)
		for _, k := range slices.Sorted(maps.Keys(hidden))[:min(len(hidden), 5)] {
			d.Commands = append(d.Commands, kubectl(clusterName, ns, fmt.Sprintf("get secret %s -o go-template='{{index .data %q | base64decode}}'", name, k)))
		}
	}
	if res.Group == "cert-manager.io" && res.Resource == "certificates" {
		d.Commands = append(d.Commands, "cmctl status certificate "+name+" -n "+shellQuote(ns)+" --context "+shellQuote(clusterName))
	}
	return d, nil
}

// redactObject strips noise and anything that can carry secret values from a raw object: managed
// fields, the last-applied annotation (which holds a full copy of what was applied), and Secret data.
func redactObject(obj map[string]any) {
	if md, ok := obj["metadata"].(map[string]any); ok {
		delete(md, "managedFields")
		if ann, ok := md["annotations"].(map[string]any); ok {
			delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
		}
	}
	if obj["kind"] != "Secret" {
		return
	}
	hidden := map[string]any{}
	for _, field := range []string{"data", "stringData"} {
		if m, ok := obj[field].(map[string]any); ok {
			for k := range m {
				hidden[k] = "<hidden by k8s-buddy>"
			}
		}
		delete(obj, field)
	}
	obj["stringData"] = hidden
}

var builtinGroupKinds = map[string]schema.GroupKind{
	"pods": {Kind: "Pod"}, "services": {Kind: "Service"}, "pvcs": {Kind: "PersistentVolumeClaim"}, "nodes": {Kind: "Node"},
	"configmaps": {Kind: "ConfigMap"}, "secrets": {Kind: "Secret"}, "events": {Kind: "Event"},
	"deployments": {Group: "apps", Kind: "Deployment"}, "statefulsets": {Group: "apps", Kind: "StatefulSet"},
	"daemonsets": {Group: "apps", Kind: "DaemonSet"},
}

// Describe returns `kubectl describe` output for any kind, custom resources included.
func (m *Manager) Describe(clusterName, kind, ns, name string) (string, error) {
	c, err := m.cluster(clusterName)
	if err != nil {
		return "", err
	}
	if kind == "secrets" {
		return c.describeSecret(ns, name)
	}
	cfg := rest.CopyConfig(c.cfg)
	cfg.Timeout = 30 * time.Second
	var d describe.ResourceDescriber
	if gk, ok := builtinGroupKinds[kind]; ok {
		d, ok = describe.DescriberFor(gk, cfg)
		if !ok {
			return "", fmt.Errorf("no describer for %s", kind)
		}
	} else {
		g, ok := genericFor(kind)
		if !ok {
			return "", fmt.Errorf("unknown kind %q", kind)
		}
		res, ok := c.findResource(g.res.Group, g.res.Resource)
		if !ok {
			return "", fmt.Errorf("%s not served by %s", g.res.Resource, clusterName)
		}
		gk := schema.GroupKind{Group: res.Group, Kind: res.Kind}
		if d, ok = describe.DescriberFor(gk, cfg); !ok {
			scope := meta.RESTScopeRoot
			if res.Namespaced {
				scope = meta.RESTScopeNamespace
			}
			mapping := &meta.RESTMapping{Resource: res.gvr(), GroupVersionKind: gk.WithVersion(res.Version), Scope: scope}
			if d, ok = describe.GenericDescriberFor(mapping, cfg); !ok {
				return "", fmt.Errorf("no describer for %s", res.Kind)
			}
		}
	}
	return d.Describe(ns, name, describe.DescriberSettings{ShowEvents: true, ChunkSize: 500})
}

// describeSecret stands in for kubectl's Secret describer, which prints a service account token
// in full. Ours never prints a value: keys and their sizes only.
func (c *cluster) describeSecret(ns, name string) (string, error) {
	if !validObjectName(ns) || !validObjectName(name) || c.cs == nil {
		return "", fmt.Errorf("invalid secret reference")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := c.cs.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return secretDescription(s), nil
}

func secretDescription(s *corev1.Secret) string {
	var b strings.Builder
	pairs := func(title string, m map[string]string) {
		keys := slices.Sorted(maps.Keys(m))
		// The last-applied annotation holds a full copy of what was applied, values included.
		keys = slices.DeleteFunc(keys, func(k string) bool { return k == corev1.LastAppliedConfigAnnotation })
		if len(keys) == 0 {
			fmt.Fprintf(&b, "%-13s<none>\n", title+":")
			return
		}
		for i, k := range keys {
			label := ""
			if i == 0 {
				label = title + ":"
			}
			fmt.Fprintf(&b, "%-13s%s=%s\n", label, k, truncate(m[k], 200))
		}
	}
	fmt.Fprintf(&b, "%-13s%s\n%-13s%s\n", "Name:", s.Name, "Namespace:", s.Namespace)
	pairs("Labels", s.Labels)
	pairs("Annotations", s.Annotations)
	fmt.Fprintf(&b, "\n%-13s%s\n\nData\n====\n", "Type:", s.Type)
	for _, k := range slices.Sorted(maps.Keys(s.Data)) {
		fmt.Fprintf(&b, "%s:  %d bytes\n", k, len(s.Data[k]))
	}
	for _, k := range slices.Sorted(maps.Keys(s.StringData)) {
		fmt.Fprintf(&b, "%s:  %d bytes\n", k, len(s.StringData[k]))
	}
	return b.String()
}
