package monitor

import (
	"strconv"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
