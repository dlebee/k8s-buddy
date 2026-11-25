package monitor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

const forbiddenBody = `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"secrets is forbidden: User \"a\" cannot list resource \"secrets\" in API group \"\" at the cluster scope","reason":"Forbidden","details":{"kind":"secrets"},"code":403}`

// A list the account may not do is asked once; repeats within the hour are answered locally
// with the same refusal, reads of one object still go through, and after the hour it asks again.
func TestDenialCacheAnswersRepeatsLocally(t *testing.T) {
	var lists, gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/secrets") {
			lists.Add(1)
		} else {
			gets.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(forbiddenBody))
	}))
	defer srv.Close()

	dc := newDenialCache()
	now := time.Now()
	dc.now = func() time.Time { return now }
	cfg := &rest.Config{Host: srv.URL}
	cfg.Wrap(dc.wrap)
	cs := kubernetes.NewForConfigOrDie(cfg)
	ctx := context.Background()
	list := func() error {
		_, err := cs.CoreV1().Secrets("").List(ctx, metav1.ListOptions{LabelSelector: "owner=helm", Limit: 500})
		return err
	}

	for range 5 {
		if err := list(); !apierrors.IsForbidden(err) {
			t.Fatalf("want forbidden, got %v", err)
		}
	}
	if err := list(); !strings.Contains(err.Error(), "k8s-buddy asks again at") {
		t.Fatalf("message should say when we ask again: %v", err)
	}
	if lists.Load() != 1 {
		t.Fatalf("API server saw %d lists, want 1", lists.Load())
	}
	for range 2 {
		_, _ = cs.CoreV1().Secrets("ns").Get(ctx, "a", metav1.GetOptions{})
	}
	if gets.Load() != 2 {
		t.Fatalf("single reads should not be cached: %d", gets.Load())
	}
	// Other selectors are their own question.
	_, _ = cs.CoreV1().Secrets("").List(ctx, metav1.ListOptions{LabelSelector: "owner=helm,name=web"})
	if lists.Load() != 2 {
		t.Fatalf("different selector: %d", lists.Load())
	}
	now = now.Add(denialTTL + time.Minute)
	_ = list()
	if lists.Load() != 3 {
		t.Fatalf("after the hour it should ask again: %d", lists.Load())
	}
}

// Allowed lists are never remembered.
func TestDenialCacheIgnoresSuccess(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"SecretList","apiVersion":"v1","metadata":{},"items":[]}`))
	}))
	defer srv.Close()
	cfg := &rest.Config{Host: srv.URL}
	cfg.Wrap(newDenialCache().wrap)
	cs := kubernetes.NewForConfigOrDie(cfg)
	for range 3 {
		if _, err := cs.CoreV1().Secrets("").List(context.Background(), metav1.ListOptions{Limit: 500}); err != nil {
			t.Fatal(err)
		}
	}
	if n.Load() != 3 {
		t.Fatalf("got %d requests", n.Load())
	}
}

// The Helm secrets informer, refused: client-go retries every second or so, but only the
// first attempt reaches the API server.
func TestRefusedInformerAsksOnce(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(forbiddenBody))
	}))
	defer srv.Close()
	cfg := &rest.Config{Host: srv.URL}
	cfg.Wrap(newDenialCache().wrap)
	mc, err := metadata.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var tries atomic.Int32
	mf := metadatainformer.NewFilteredSharedInformerFactory(mc, 0, "", func(o *metav1.ListOptions) { o.LabelSelector = "owner=helm" })
	inf := mf.ForResource(corev1.SchemeGroupVersion.WithResource("secrets")).Informer()
	_ = inf.SetWatchErrorHandler(func(_ *cache.Reflector, err error) { tries.Add(1) })
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	mf.Start(ctx.Done())
	<-ctx.Done()
	if tries.Load() < 2 || n.Load() != 1 {
		t.Fatalf("informer tried %d times, API server saw %d", tries.Load(), n.Load())
	}
}
