package monitor

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testPod(node string, phase corev1.PodPhase, cpuReq, memReq string) *corev1.Pod {
	rl := corev1.ResourceList{}
	if cpuReq != "" {
		rl[corev1.ResourceCPU] = resource.MustParse(cpuReq)
	}
	if memReq != "" {
		rl[corev1.ResourceMemory] = resource.MustParse(memReq)
	}
	return &corev1.Pod{Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{
		{Name: "a", Resources: corev1.ResourceRequirements{Requests: rl}}, {Name: "sidecar"}}},
		Status: corev1.PodStatus{Phase: phase}}
}

func TestNodeRequestsSumsLivePodsPerNode(t *testing.T) {
	req, _ := nodeRequests([]*corev1.Pod{
		testPod("n1", corev1.PodRunning, "500m", "1Gi"),
		testPod("n1", corev1.PodPending, "250m", ""),   // memory unset counts as zero
		testPod("n1", corev1.PodSucceeded, "4", "8Gi"), // finished pods free their requests
		testPod("n2", corev1.PodRunning, "1", "512Mi"),
		testPod("", corev1.PodPending, "2", "2Gi"), // not scheduled yet
	})
	if got := req["n1"]; got.CPU != 750 || got.Memory != 1<<30 {
		t.Fatalf("n1: %+v", got)
	}
	if got := req["n2"]; got.CPU != 1000 || got.Memory != 512<<20 {
		t.Fatalf("n2: %+v", got)
	}
	if len(req) != 2 {
		t.Fatalf("unexpected nodes: %v", req)
	}
}

func TestNodeRowUsageAndSort(t *testing.T) {
	node := func(name, cpu string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse("16Gi")}}}
	}
	req := map[string]ResourceNumbers{"a": {CPU: 3000, Memory: 1 << 30}}
	ua := nodeRowUsage(node("a", "3860m"), req, true)
	if ua.CPULim != 3860 || ua.MemLim != 16<<30 || ua.CPUReq != 3000 || ua.CPU != -1 {
		t.Fatalf("a: %+v", ua)
	}
	if ub := nodeRowUsage(node("b", "4"), req, true); ub.CPUReq != 0 {
		t.Fatalf("node without pods requests nothing: %+v", ub)
	}
	if uc := nodeRowUsage(node("c", "4"), req, false); uc.CPUReq != -1 {
		t.Fatalf("pods not synced means unknown requests: %+v", uc)
	}

	// Heaviest first across clusters; a cluster without metrics sorts by requests after live rows.
	rows := []Resource{
		{Cluster: "x", Name: "idle", Usage: &RowUsage{CPU: 100, Mem: 1, CPUReq: 3000}},
		{Cluster: "y", Name: "busy", Usage: &RowUsage{CPU: 2500, Mem: 1, CPUReq: 500}},
		{Cluster: "z", Name: "nometrics", Usage: &RowUsage{CPU: -1, Mem: -1, CPUReq: 9000}},
		{Cluster: "x", Name: "mid", Usage: &RowUsage{CPU: 900, Mem: 1, CPUReq: 100}},
	}
	out := newResourceList("nodes", nil, false)
	out.apply(rows, ResourceQuery{Kind: "nodes", Sort: "cpu"})
	var names []string
	for _, r := range out.Items {
		names = append(names, r.Name)
	}
	if want := "busy mid idle nometrics"; strings.Join(names, " ") != want {
		t.Fatalf("order %v, want %s", names, want)
	}
}
