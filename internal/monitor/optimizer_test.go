package monitor

import (
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
	if r.Verdict != "missing" || r.Confidence != "high" || r.CPU.Request.Recommended != 50 || r.CPU.Limit.Recommended != 275 ||
		r.Mem.Request.Recommended != 120<<20 || r.Mem.Limit.Recommended != 232<<20 {
		t.Fatalf("missing: %+v %+v %+v", r.Verdict, r.CPU, r.Mem)
	}
	if !strings.Contains(r.Command, "set resources deployment/api -c api --requests=cpu=50m,memory=120Mi --limits=cpu=275m,memory=232Mi") {
		t.Fatalf("command: %s", r.Command)
	}

	// Memory peak near the limit without OOM kills: sizes itself to its limit. Kept, not at risk.
	ct := corev1.Container{Name: "api", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("190Mi")}}}
	if r := optRow(base, ct, week, history.OOMStat{}, false); r.Verdict == "risk" || !r.FillsLimit || r.Mem.Limit.Recommended != 190<<20 {
		t.Fatalf("fills limit: %s %v %d %+v", r.Verdict, r.FillsLimit, r.Mem.Limit.Recommended>>20, r.Findings)
	}
	// Frequent OOM kills (3 within a day): at risk, and the limit is raised from the limit itself.
	if r := optRow(base, ct, week, history.OOMStat{Count: 3, Last: time.Now(), MaxDay: 3}, false); r.Verdict != "risk" || r.OOMRare || r.FillsLimit || r.Mem.Limit.Recommended != 240<<20 {
		t.Fatalf("frequent oom: %s %d %+v", r.Verdict, r.Mem.Limit.Recommended>>20, r.Findings)
	}
	// More than once a week per replica, spread out: also frequent.
	if r := optRow(base, ct, week, history.OOMStat{Count: 7, Last: time.Now(), MaxDay: 1}, false); r.Verdict != "risk" || r.OOMRare {
		t.Fatalf("weekly+ oom: %s %+v", r.Verdict, r.Findings)
	}
	// Rare (3 kills over 8 days on 3 replicas, never a burst): a notice, and the limit is kept.
	if r := optRow(base, ct, week, history.OOMStat{Count: 3, Last: time.Now(), MaxDay: 1}, false); r.Verdict == "risk" || !r.OOMRare || r.Mem.Limit.Recommended != 190<<20 ||
		len(r.Findings) == 0 || !strings.Contains(r.Findings[0].Text, "Rare") || !strings.Contains(r.Findings[0].Text, "other replicas") {
		t.Fatalf("rare oom: %s %d %+v", r.Verdict, r.Mem.Limit.Recommended>>20, r.Findings)
	}
	// One replica: rare kills are still short outages, and the notice says so.
	one := base
	one.Replicas = 1
	if r := optRow(one, ct, week, history.OOMStat{Count: 1, Last: time.Now(), MaxDay: 1}, false); !r.OOMRare || !strings.Contains(r.Findings[0].Text, "short outage") {
		t.Fatalf("one replica: %+v", r.Findings)
	}
	// No memory limit: the node ran out of memory, a risk however rare.
	noLim := corev1.Container{Name: "api", Resources: corev1.ResourceRequirements{Requests: ct.Resources.Requests}}
	if r := optRow(base, noLim, week, history.OOMStat{Count: 1, Last: time.Now(), MaxDay: 1}, false); r.Verdict != "risk" || r.OOMRare {
		t.Fatalf("no limit: %s %+v", r.Verdict, r.Findings)
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
