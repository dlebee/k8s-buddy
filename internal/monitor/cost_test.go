package monitor

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestPack(t *testing.T) {
	// Three 600m pods on nodes with 1000m of room: sums to 1.8 nodes but needs 3 (they don't pair).
	items := []packItem{{cpu: 600, mem: 1}, {cpu: 600, mem: 1}, {cpu: 600, mem: 1}}
	if n, why := pack(items, 1000, 100, false); n != 3 || why != "cpu" {
		t.Fatalf("got %d %s", n, why)
	}
	// By usage they are small: everything fits on one node.
	for i := range items {
		items[i].ucpu, items[i].umem = 100, 1
	}
	if n, _ := pack(items, 1000, 100, true); n != 1 {
		t.Fatalf("by usage got %d", n)
	}
	// Memory-bound.
	if n, why := pack([]packItem{{cpu: 1, mem: 60}, {cpu: 1, mem: 60}}, 1000, 100, false); n != 2 || why != "memory" {
		t.Fatalf("memory got %d %s", n, why)
	}
}

func TestPinnedPod(t *testing.T) {
	if pinnedPod(&corev1.Pod{Spec: corev1.PodSpec{NodeSelector: map[string]string{"kubernetes.io/os": "linux"}}}) {
		t.Fatal("os selector is not pinning")
	}
	if !pinnedPod(&corev1.Pod{Spec: corev1.PodSpec{NodeSelector: map[string]string{"agentpool": "gpu"}}}) {
		t.Fatal("pool selector pins")
	}
}

func TestSystemNamespace(t *testing.T) {
	for ns, want := range map[string]bool{"kube-system": true, "cert-manager": true, "nginx-ingress": true, "monitoring": true,
		"backube-snapscheduler": true, "metacontroller": true, "shop": false, "payments": false, "api": false} {
		if systemNamespace(ns) != want {
			t.Errorf("%s: got %v want %v", ns, !want, want)
		}
	}
}
