package monitor

import (
	"errors"
	"io"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Watches are asked to stay open 25 to 35 minutes; lists (no timeout set) are left alone.
func TestLongWatch(t *testing.T) {
	list := metav1.ListOptions{}
	longWatch(&list)
	if list.TimeoutSeconds != nil {
		t.Fatalf("a list got a timeout: %d", *list.TimeoutSeconds)
	}
	for range 200 {
		short := int64(300)
		w := metav1.ListOptions{TimeoutSeconds: &short}
		longWatch(&w)
		if got := *w.TimeoutSeconds; got < 25*60 || got >= 35*60 {
			t.Fatalf("watch timeout %ds, want 25 to 35 minutes", got)
		}
		if short != 300 {
			t.Fatal("the caller's value was changed in place")
		}
	}
}

// A watch that fails without an answer from the cluster asks for a connection check now, at most
// once a minute; a refusal from the API server or a watch that simply ended doesn't.
func TestWatchFailedRechecksConnection(t *testing.T) {
	c := newCluster("a", "https://a.example.com", nil, nil)
	kicked := func() bool {
		select {
		case <-c.retry:
			return true
		default:
			return false
		}
	}
	c.watchFailed("pods", apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("no")))
	c.watchFailed("pods", io.EOF)
	if kicked() {
		t.Fatal("an answered or ended watch asked for a connection check")
	}
	c.watchFailed("pods", errors.New("dial tcp 192.0.2.1:443: i/o timeout"))
	if !kicked() {
		t.Fatal("an unanswered watch didn't ask for a connection check")
	}
	c.watchFailed("nodes", errors.New("dial tcp 192.0.2.1:443: i/o timeout"))
	if kicked() {
		t.Fatal("asked again within a minute")
	}
	if c.resourceError("nodes") == "" {
		t.Fatal("the failure wasn't recorded")
	}
}
