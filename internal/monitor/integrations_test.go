package monitor

import (
	"strconv"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	appslisters "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/dlebee/k8s-buddy/internal/alert"
)

func lister(t *testing.T, objs ...any) cache.GenericLister {
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, o := range objs {
		if err := idx.Add(o); err != nil {
			t.Fatal(err)
		}
	}
	return cache.NewGenericLister(idx, schema.GroupResource{})
}

func cert(name string, notAfter, renewal time.Time, ready string, readySince time.Time) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "namespace": "ns"},
		"status": map[string]any{
			"notAfter":    notAfter.UTC().Format(time.RFC3339),
			"renewalTime": renewal.UTC().Format(time.RFC3339),
			"conditions": []any{map[string]any{"type": "Ready", "status": ready, "reason": "Pending",
				"lastTransitionTime": readySince.UTC().Format(time.RFC3339)}},
		},
	}}
}

func TestCertManagerDetection(t *testing.T) {
	now := time.Now()
	c := &cluster{name: "c", dynListers: map[string]cache.GenericLister{
		"cm-certificates": lister(t,
			cert("expired", now.Add(-time.Hour), now.Add(-3*time.Hour), "False", now.Add(-time.Hour)),
			cert("overdue-urgent", now.Add(24*time.Hour), now.Add(-2*time.Hour), "True", now.Add(-48*time.Hour)),
			cert("overdue", now.Add(20*24*time.Hour), now.Add(-2*time.Hour), "True", now.Add(-48*time.Hour)),
			cert("short-lived-ok", now.Add(40*time.Minute), now.Add(20*time.Minute), "True", now.Add(-time.Minute)),
			cert("stuck", now.Add(30*24*time.Hour), now.Add(10*24*time.Hour), "False", now.Add(-time.Hour)),
		),
	}}
	got := map[string]alert.Severity{}
	for _, a := range c.detectCertManager() {
		got[a.Key] = a.Severity
	}
	want := map[string]alert.Severity{
		"cert-expired/ns/expired":                alert.Critical,
		"cert-not-ready/ns/expired":              alert.Warning,
		"cert-renewal-overdue/ns/overdue-urgent": alert.Critical,
		"cert-renewal-overdue/ns/overdue":        alert.Warning,
		"cert-not-ready/ns/stuck":                alert.Warning,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q want %q (all: %v)", k, got[k], v, got)
		}
	}
}

func helmSecret(name, status string, rev int, modified time.Time) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
		Name: "sh.helm.release.v1." + name + ".v" + strconv.Itoa(rev), Namespace: "ns",
		Labels: map[string]string{"owner": "helm", "name": name, "status": status, "version": strconv.Itoa(rev),
			"modifiedAt": strconv.FormatInt(modified.Unix(), 10)},
	}}
}

func TestHelmDetection(t *testing.T) {
	now := time.Now()
	c := &cluster{name: "c", helm: lister(t,
		helmSecret("ok", "superseded", 1, now.Add(-time.Hour)), helmSecret("ok", "deployed", 2, now.Add(-time.Minute)),
		helmSecret("broken", "failed", 3, now.Add(-time.Hour)),
		helmSecret("stuck", "pending-upgrade", 5, now.Add(-time.Hour)),
		helmSecret("busy", "pending-install", 1, now.Add(-time.Minute)),
	)}
	got := map[string]bool{}
	for _, a := range c.detectHelm() {
		got[a.Key] = true
	}
	if len(got) != 2 || !got["helm-failed/ns/broken"] || !got["helm-stuck/ns/stuck"] {
		t.Fatalf("got %v", got)
	}
	for _, r := range listHelm(c) {
		if r.Name == "ok" && (r.Status != "deployed" || r.Cols[0] != "2") {
			t.Fatalf("latest revision not picked: %+v", r)
		}
	}
}

func helmDeploy(release, namespace, chart, name string, created time.Time) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: namespace, CreationTimestamp: metav1.NewTime(created),
		Labels:      map[string]string{"app.kubernetes.io/managed-by": "Helm", "helm.sh/chart": chart},
		Annotations: map[string]string{"meta.helm.sh/release-name": release, "meta.helm.sh/release-namespace": namespace},
	}}
}

func deployLister(t *testing.T, objs ...*appsv1.Deployment) appslisters.DeploymentLister {
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, o := range objs {
		if err := idx.Add(o); err != nil {
			t.Fatal(err)
		}
	}
	return appslisters.NewDeploymentLister(idx)
}

func TestHelmFallbackFromWorkloads(t *testing.T) {
	now := time.Now()
	c := &cluster{name: "c", deploys: deployLister(t,
		helmDeploy("api", "prod", "api-1.2.3", "api", now.Add(-time.Hour)),
		helmDeploy("api", "prod", "api-1.2.3", "api-worker", now.Add(-time.Minute)),
		helmDeploy("ui", "prod", "ui-0.1.0", "ui", now.Add(-10*time.Minute)),
	)}
	rels := c.helmReleases()
	if len(rels) != 2 {
		t.Fatalf("want 2 releases, got %d: %+v", len(rels), rels)
	}
	byName := map[string]helmRelease{}
	for _, r := range rels {
		byName[r.name] = r
	}
	api := byName["api"]
	if api.status != "unknown" || api.chart != "api-1.2.3" || len(api.workloads) != 2 {
		t.Fatalf("api release: %+v", api)
	}
	// Alerts must not fire for fallback releases (status unknown).
	if alerts := c.detectHelm(); len(alerts) != 0 {
		t.Fatalf("no alerts should fire for fallback releases, got %+v", alerts)
	}
	// List rows carry "?" revision for fallback discoveries.
	for _, r := range listHelm(c) {
		if r.Cols[0] != "?" {
			t.Fatalf("fallback revision should be ?, got %q (%+v)", r.Cols[0], r)
		}
	}
}

func TestHelmReleasesPrefersSecretOverWorkloads(t *testing.T) {
	now := time.Now()
	c := &cluster{name: "c",
		helm: lister(t, helmSecret("api", "deployed", 3, now.Add(-time.Minute))),
		deploys: deployLister(t,
			helmDeploy("api", "ns", "api-1.2.3", "api", now.Add(-time.Hour)),
		),
	}
	rels := c.helmReleases()
	if len(rels) != 1 || rels[0].name != "api" || rels[0].status != "deployed" || rels[0].revision != 3 {
		t.Fatalf("secret data should win: %+v", rels)
	}
}
