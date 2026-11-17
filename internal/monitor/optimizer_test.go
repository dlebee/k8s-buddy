package monitor

import (
	"strings"
	"testing"

	"github.com/dlebee/k8s-buddy/internal/history"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestOptimizerRounding(t *testing.T) {
	for in, want := range map[float64]int64{1: 25, 26: 50, 74: 75, 101: 125, 999: 1000, 1001: 1250, 1600: 1750} {
		if got := roundCPU(in); got != want {
			t.Errorf("roundCPU(%v) = %d, want %d", in, got, want)
		}
	}
	for in, want := range map[float64]int64{1: 16 << 20, 100 << 20: 112 << 20, 128 << 20: 128 << 20, 300 << 20: 320 << 20, 600 << 20: 640 << 20, 5.5 * (1 << 30): 6 << 30} {
		if got := roundMem(in); got != want {
			t.Errorf("roundMem(%v) = %d, want %d", in, got, want)
		}
	}
	if memQty(1<<30) != "1Gi" || memQty(512<<20) != "512Mi" || cpuQty(1250) != "1250m" || cpuQty(2000) != "2" {
		t.Error("quantities")
	}
}

func TestOptimizerRecommendations(t *testing.T) {
	week := history.Profile{Buckets: 8 * 288, First: 0, Last: 8 * 86400, // 8 days, no gaps
		CPUMed: 40, CPUP95: 90, CPUMax: 200, MemMed: 100 << 20, MemP95: 120 << 20, MemMax: 180 << 20}
	base := OptRow{Cluster: "c", Namespace: "web", Kind: "Deployment", Workload: "api", Replicas: 3}

	// No requests or limits: recommend both from median and max.
	r := optRow(base, corev1.Container{Name: "api"}, week, false)
	if r.Verdict != "missing" || r.Confidence != "high" || r.CPU.Request.Recommended != 50 || r.CPU.Limit.Recommended != 275 ||
		r.Mem.Request.Recommended != 128<<20 || r.Mem.Limit.Recommended != 256<<20 {
		t.Fatalf("missing: %+v %+v %+v", r.Verdict, r.CPU, r.Mem)
	}
	if !strings.Contains(r.Command, "set resources deployment/api -c api --requests=cpu=50m,memory=128Mi --limits=cpu=275m,memory=256Mi") {
		t.Fatalf("command: %s", r.Command)
	}

	// Memory peak near the limit: at risk.
	ct := corev1.Container{Name: "api", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("190Mi")}}}
	if r := optRow(base, ct, week, false); r.Verdict != "risk" {
		t.Fatalf("risk: %+v", r.Findings)
	}

	// Typical memory above the request but well under the limit: under-requested, not at risk.
	ct.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("64Mi")
	ct.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("1Gi")
	if r := optRow(base, ct, week, false); r.Verdict != "under" {
		t.Fatalf("under: %s %+v", r.Verdict, r.Findings)
	}

	// CPU above its request only: a note, the verdict stays fine.
	ct.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("20m")
	ct.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("128Mi")
	if r := optRow(base, ct, week, false); r.Verdict != "ok" {
		t.Fatalf("cpu above request: %s %+v", r.Verdict, r.Findings)
	}

	// Requests far above use: over-provisioned, and freed requests count every replica.
	ct.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("1")
	ct.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("1Gi")
	ct.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("2Gi")
	r = optRow(base, ct, week, false)
	if r.Verdict != "over" || r.FreeCPU != (1000-50)*3 || r.FreeMem != (1024-128)*3<<20 {
		t.Fatalf("over: %s %d %d %+v", r.Verdict, r.FreeCPU, r.FreeMem, r.Findings)
	}

	// Two days of data over a five-day span: medium confidence, a wait warning, and the gap noted.
	short := week
	short.Buckets, short.Last = 2*288, 5*86400
	r = optRow(base, ct, short, false)
	text := ""
	for _, f := range r.Findings {
		text += f.Text + "\n"
	}
	if r.Confidence != "medium" || !strings.Contains(text, "better to wait") || !strings.Contains(text, "skipped") {
		t.Fatalf("short: %s %s", r.Confidence, text)
	}
}
