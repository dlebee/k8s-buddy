package monitor

import (
	"slices"
	"strings"
	"testing"
	"time"

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
	for in, want := range map[float64]int64{1: 16 << 20, 100 << 20: 104 << 20, 101 << 20: 104 << 20, 300 << 20: 304 << 20, 1.3 * (1 << 30): 1408 << 20, 6.24 * (1 << 30): 6400 << 20, 9 * (1 << 30): 9 << 30, 9.1 * (1 << 30): 9728 << 20} {
		if got := roundMem(in); got != want {
			t.Errorf("roundMem(%v) = %d, want %d", in, got, want)
		}
	}
	// A large stateful workload: typical 5.2Gi, max 5.5Gi → request 6.25Gi, limit 6.875Gi (not 7Gi/10Gi).
	big := history.Profile{Minutes: 12000, Last: 8 * 86400, CPUMed: 147, CPUMax: 1260, MemMed: 5.2 * (1 << 30), MemMax: 5.5 * (1 << 30)}
	r := optRow(OptRow{Kind: "StatefulSet", Workload: "db", Replicas: 2}, corev1.Container{Name: "db"}, big, history.OOMStat{}, false)
	if r.Mem.Request.Recommended != 6400<<20 || r.Mem.Limit.Recommended != 7040<<20 || r.CPU.Request.Recommended != 200 || r.CPU.Limit.Recommended != 1750 {
		t.Errorf("big: mem %d/%d MiB cpu %d/%d", r.Mem.Request.Recommended>>20, r.Mem.Limit.Recommended>>20, r.CPU.Request.Recommended, r.CPU.Limit.Recommended)
	}
	if memQty(1<<30) != "1Gi" || memQty(512<<20) != "512Mi" || cpuQty(1250) != "1250m" || cpuQty(2000) != "2" {
		t.Error("quantities")
	}
}

func TestOptimizerRecommendations(t *testing.T) {
	week := history.Profile{Minutes: 8 * 1440, First: 0, Last: 8 * 86400, // 8 days, no gaps
		CPUMed: 40, CPUP95: 90, CPUMax: 200, MemMed: 100 << 20, MemP95: 120 << 20, MemMax: 180 << 20}
	base := OptRow{Cluster: "c", Namespace: "web", Kind: "Deployment", Workload: "api", Replicas: 3}

	// No requests or limits: recommend both from median and max.
	r := optRow(base, corev1.Container{Name: "api"}, week, history.OOMStat{}, false)
	if r.Verdict != "missing" || r.Confidence != "high" || r.CPU.Request.Recommended != 50 || r.CPU.Limit.Recommended != 300 ||
		r.Mem.Request.Recommended != 120<<20 || r.Mem.Limit.Recommended != 232<<20 {
		t.Fatalf("missing: %+v %+v %+v", r.Verdict, r.CPU, r.Mem)
	}
	if !strings.Contains(r.Command, "set resources deployment/api -c api --requests=cpu=50m,memory=120Mi --limits=cpu=300m,memory=232Mi") {
		t.Fatalf("command: %s", r.Command)
	}

	// Memory peak near the limit without OOM kills: sizes itself to its limit. Kept, not at risk.
	ct := corev1.Container{Name: "api", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("190Mi")}}}
	if r := optRow(base, ct, week, history.OOMStat{}, false); r.Verdict == "risk" || !r.FillsLimit || r.Mem.Limit.Recommended != 190<<20 {
		t.Fatalf("fills limit: %s %v %d %+v", r.Verdict, r.FillsLimit, r.Mem.Limit.Recommended>>20, r.Findings)
	}
	// 2 kills within 2 days: at risk, and the limit is raised from the limit itself.
	if r := optRow(base, ct, week, history.OOMStat{Count: 2, Last: time.Now(), Max2Days: 2}, false); r.Verdict != "risk" || r.OOMs != 2 || r.OOMBurst != 2 || r.FillsLimit || r.Mem.Limit.Recommended != 240<<20 {
		t.Fatalf("2 in 2 days: %s %d %+v", r.Verdict, r.Mem.Limit.Recommended>>20, r.Findings)
	}
	// Kills spread out (never 2 within 2 days): not mentioned, and the limit it was killed at is kept.
	if r := optRow(base, ct, week, history.OOMStat{Count: 4, Last: time.Now(), Max2Days: 1}, false); r.Verdict == "risk" || r.OOMs != 0 || r.FillsLimit || r.Mem.Limit.Recommended != 190<<20 ||
		slices.ContainsFunc(r.Findings, func(f OptFinding) bool { return strings.Contains(f.Text, "OOM") }) {
		t.Fatalf("spread out: %s %d %+v", r.Verdict, r.Mem.Limit.Recommended>>20, r.Findings)
	}
	// No memory limit: the same rule.
	noLim := corev1.Container{Name: "api", Resources: corev1.ResourceRequirements{Requests: ct.Resources.Requests}}
	if r := optRow(base, noLim, week, history.OOMStat{Count: 2, Last: time.Now(), Max2Days: 2}, false); r.Verdict != "risk" {
		t.Fatalf("no limit, 2 in 2 days: %s %+v", r.Verdict, r.Findings)
	}
	if r := optRow(base, noLim, week, history.OOMStat{Count: 1, Last: time.Now(), Max2Days: 1}, false); r.OOMs != 0 {
		t.Fatalf("no limit, once: %+v", r.Findings)
	}

	// Typical memory above the request but well under the limit: under-requested, not at risk.
	ct.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("64Mi")
	ct.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("1Gi")
	if r := optRow(base, ct, week, history.OOMStat{}, false); r.Verdict != "under" {
		t.Fatalf("under: %s %+v", r.Verdict, r.Findings)
	}

	// CPU above its request only: a note, the verdict stays fine.
	ct.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("20m")
	ct.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("128Mi")
	if r := optRow(base, ct, week, history.OOMStat{}, false); r.Verdict != "ok" {
		t.Fatalf("cpu above request: %s %+v", r.Verdict, r.Findings)
	}

	// Requests far above use: over-provisioned, and freed requests count every replica.
	ct.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("1")
	ct.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("1Gi")
	ct.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("2Gi")
	r = optRow(base, ct, week, history.OOMStat{}, false)
	if r.Verdict != "over" || r.FreeCPU != (1000-50)*3 || r.FreeMem != (1024-120)*3<<20 {
		t.Fatalf("over: %s %d %d %+v", r.Verdict, r.FreeCPU, r.FreeMem, r.Findings)
	}

	// Two days of data over a five-day span: medium confidence, a wait warning, and the gap noted.
	short := week
	short.Minutes, short.Last = 2*1440, 5*86400
	r = optRow(base, ct, short, history.OOMStat{}, false)
	text := ""
	for _, f := range r.Findings {
		text += f.Text + "\n"
	}
	if r.Confidence != "medium" || !strings.Contains(text, "better to wait") || !strings.Contains(text, "skipped") {
		t.Fatalf("short: %s %s", r.Confidence, text)
	}
}

// A CPU limit only throttles. A one-off spike to the limit (a restart, say) isn't a risk and isn't
// mentioned; sitting at it a real share of the time is a notice, still not a risk.
func TestCPULimitIsNeverARisk(t *testing.T) {
	ct := corev1.Container{Name: "rpc", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("3Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")}}}
	base := OptRow{Cluster: "c", Namespace: "ns", Kind: "StatefulSet", Workload: "rpc", Replicas: 2}
	cpuNote := func(r OptRow) bool {
		return slices.ContainsFunc(r.Findings, func(f OptFinding) bool { return strings.Contains(f.Text, "throttled") })
	}
	spike := history.Profile{Minutes: 8 * 1440, Last: 8 * 86400, CPUMed: 18, CPUP95: 25, CPUMax: 1900, MemMed: 2400 << 20, MemP95: 3300 << 20, MemMax: 3600 << 20}
	if r := optRow(base, ct, spike, history.OOMStat{}, false); r.Verdict == "risk" || cpuNote(r) {
		t.Fatalf("one spike: %s %+v", r.Verdict, r.Findings)
	}
	busy := spike
	busy.CPUMed, busy.CPUP95, busy.CPUMax = 1200, 1950, 2000
	if r := optRow(base, ct, busy, history.OOMStat{}, false); r.Verdict == "risk" || !cpuNote(r) {
		t.Fatalf("at the limit often: %s %+v", r.Verdict, r.Findings)
	}
}
