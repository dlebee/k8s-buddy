package monitor

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dlebee/k8s-buddy/internal/history"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// Resource Quotas Optimizer: suggests requests and limits per container from recorded usage.
// Requests follow the median (typical) usage, limits the highest reading, each with headroom.
// Only the time k8s-buddy was recording counts: gaps are skipped, not read as zero usage.

const (
	optGoodDays   = 7.0 // below this much recorded data, recommendations carry a warning
	optMinBuckets = 12  // an hour of data before anything is suggested
)

type OptValue struct {
	Current     int64 `json:"current"`     // -1 not set
	Recommended int64 `json:"recommended"` // per replica
}

type OptResource struct {
	Request OptValue `json:"request"`
	Limit   OptValue `json:"limit"`
	Median  float64  `json:"median"` // typical usage per replica
	P95     float64  `json:"p95"`
	Max     float64  `json:"max"` // highest reading
}

type OptFinding struct {
	Level string `json:"level"` // bad | warn | notice | info
	Text  string `json:"text"`
}

type OptRow struct {
	Cluster     string       `json:"cluster"`
	Namespace   string       `json:"namespace"`
	Kind        string       `json:"kind"`
	Workload    string       `json:"workload"`
	Container   string       `json:"container"`
	Containers  int          `json:"containers"` // in the pod
	Replicas    int          `json:"replicas"`   // running now
	System      bool         `json:"system"`
	Helm        bool         `json:"helm"`
	Days        float64      `json:"days"`     // recorded data (gaps skipped)
	SpanDays    float64      `json:"spanDays"` // first to last reading
	Confidence  string       `json:"confidence"`
	Verdict     string       `json:"verdict"` // risk | missing | under | over | ok
	CPU         OptResource  `json:"cpu"`
	Mem         OptResource  `json:"mem"`
	Findings    []OptFinding `json:"findings"`
	Command     string       `json:"command"`
	FreeCPU     int64        `json:"freeCpu"` // requests freed across replicas (negative: more needed)
	FreeMem     int64        `json:"freeMem"`
	PodLevel    bool         `json:"podLevel"` // multi-container pod sized as a whole (per-container data still collecting)
	HistoryKind string       `json:"historyKind"`
	HistoryName string       `json:"historyName"`
}

var verdictOrder = map[string]int{"risk": 0, "missing": 1, "under": 2, "over": 3, "ok": 4}

type OptReport struct {
	Available     bool     `json:"available"` // usage history on
	RetentionDays int      `json:"retentionDays"`
	Days          float64  `json:"days"` // most recorded data of any workload
	Rows          []OptRow `json:"rows"`
	Collecting    int      `json:"collecting"` // workloads with under an hour of data, not shown
	Clusters      []string `json:"clusters"`
}

type optCacheEntry struct {
	at        time.Time
	pods, cts []history.Profile
}

var (
	optCacheMu sync.Mutex
	optCache   = map[string]optCacheEntry{}
)

func (m *Manager) profiles(h *history.Store, cluster string, days int) ([]history.Profile, []history.Profile) {
	optCacheMu.Lock()
	defer optCacheMu.Unlock()
	if e, ok := optCache[cluster]; ok && time.Since(e.at) < 5*time.Minute {
		return e.pods, e.cts
	}
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	pods, err := h.Profiles(cluster, "pod", since)
	if err != nil {
		m.log.Warn("optimizer profiles", "cluster", cluster, "err", err)
	}
	cts, _ := h.Profiles(cluster, "container", since)
	optCache[cluster] = optCacheEntry{at: time.Now(), pods: pods, cts: cts}
	return pods, cts
}

// Optimize builds recommendations for the given clusters (all when empty).
func (m *Manager) Optimize(clusters []string) OptReport {
	s := m.settings.Get()
	rep := OptReport{RetentionDays: s.UsageRetentionDays, Rows: []OptRow{}, Clusters: []string{}}
	h := m.History()
	if h == nil || !s.UsageHistory {
		return rep
	}
	rep.Available = true
	for _, c := range m.connected() {
		if len(clusters) > 0 && !slices.Contains(clusters, c.name) || !c.synced("pods") {
			continue
		}
		rep.Clusters = append(rep.Clusters, c.name)
		pods, cts := m.profiles(h, c.name, s.UsageRetentionDays)
		rows, collecting := c.optimize(pods, cts)
		rep.Rows = append(rep.Rows, rows...)
		rep.Collecting += collecting
	}
	for _, r := range rep.Rows {
		rep.Days = max(rep.Days, r.Days)
	}
	order := verdictOrder
	slices.SortFunc(rep.Rows, func(a, b OptRow) int {
		if d := order[a.Verdict] - order[b.Verdict]; d != 0 {
			return d
		}
		return strings.Compare(a.Cluster+a.Namespace+a.Workload+a.Container, b.Cluster+b.Namespace+b.Workload+b.Container)
	})
	slices.Sort(rep.Clusters)
	return rep
}

func (c *cluster) optimize(podProfiles, ctProfiles []history.Profile) ([]OptRow, int) {
	byOwner := map[string]history.Profile{}
	for _, p := range podProfiles {
		byOwner[p.Namespace+"/"+p.Owner] = p
	}
	byCt := map[string]history.Profile{}
	for _, p := range ctProfiles {
		byCt[p.Namespace+"/"+p.Owner] = p
	}
	// Live workloads: the newest running pod stands for the current spec.
	type live struct {
		pod      *corev1.Pod
		replicas int
	}
	ws := map[string]*live{}
	pods, _ := c.pods.List(labels.Everything())
	for _, p := range pods {
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		kind, name := workloadOf(p)
		k := p.Namespace + "/" + kind + "/" + name
		w := ws[k]
		if w == nil {
			w = &live{}
			ws[k] = w
		}
		w.replicas++
		if w.pod == nil || p.CreationTimestamp.After(w.pod.CreationTimestamp.Time) {
			w.pod = p
		}
	}
	var rows []OptRow
	collecting := 0
	for k, w := range ws {
		prof, ok := byOwner[k]
		if !ok || prof.Buckets < optMinBuckets {
			collecting++
			continue
		}
		p := w.pod
		kind, name := workloadOf(p)
		base := OptRow{Cluster: c.name, Namespace: p.Namespace, Kind: kind, Workload: name, Containers: len(p.Spec.Containers),
			Replicas: w.replicas, System: systemNamespace(p.Namespace), Helm: p.Labels["app.kubernetes.io/managed-by"] == "Helm",
			HistoryKind: "workload", HistoryName: kind + "/" + name}
		if len(p.Spec.Containers) == 1 {
			rows = append(rows, optRow(base, p.Spec.Containers[0], prof, false))
			continue
		}
		// Several containers: per-container data when there is enough, else the pod as a whole.
		all := true
		for _, ct := range p.Spec.Containers {
			if cp, ok := byCt[k+"/"+ct.Name]; !ok || cp.Buckets < optMinBuckets {
				all = false
			}
		}
		if all {
			for _, ct := range p.Spec.Containers {
				rows = append(rows, optRow(base, ct, byCt[k+"/"+ct.Name], false))
			}
			continue
		}
		whole := corev1.Container{Name: "(whole pod)", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}}}
		r := podRequests(p)
		row := optRow(base, whole, prof, true)
		row.CPU.Request.Current, row.CPU.Limit.Current = r.CPUReq, r.CPULim
		row.Mem.Request.Current, row.Mem.Limit.Current = r.MemReq, r.MemLim
		judge(&row)
		rows = append(rows, row)
	}
	return rows, collecting
}

func optRow(row OptRow, ct corev1.Container, prof history.Profile, podLevel bool) OptRow {
	row.Container, row.PodLevel = ct.Name, podLevel
	row.Days = float64(prof.Buckets) * history.Bucket.Hours() / 24
	row.SpanDays = float64(prof.Last-prof.First)/86400 + history.Bucket.Hours()/24
	switch {
	case row.Days >= optGoodDays:
		row.Confidence = "high"
	case row.Days >= 2:
		row.Confidence = "medium"
	default:
		row.Confidence = "low"
	}
	cpuReq := roundCPU(max(prof.CPUMed*1.2, 5))
	cpuLim := roundCPU(max(prof.CPUMax*1.3, float64(cpuReq)*1.5))
	memReq := roundMem(max(prof.MemMed*1.2, 16<<20))
	memLim := roundMem(max(prof.MemMax*1.25, float64(memReq)*1.25))
	row.CPU = OptResource{Median: prof.CPUMed, P95: prof.CPUP95, Max: prof.CPUMax,
		Request: OptValue{quantity(ct.Resources.Requests, corev1.ResourceCPU, true), cpuReq},
		Limit:   OptValue{quantity(ct.Resources.Limits, corev1.ResourceCPU, true), cpuLim}}
	row.Mem = OptResource{Median: prof.MemMed, P95: prof.MemP95, Max: prof.MemMax,
		Request: OptValue{quantity(ct.Resources.Requests, corev1.ResourceMemory, false), memReq},
		Limit:   OptValue{quantity(ct.Resources.Limits, corev1.ResourceMemory, false), memLim}}
	judge(&row)
	return row
}

// judge sets findings, verdict, freed requests and the command from current vs recommended.
func judge(r *OptRow) {
	var f []OptFinding
	add := func(level, format string, a ...any) { f = append(f, OptFinding{level, fmt.Sprintf(format, a...)}) }
	cpu, mem := r.CPU, r.Mem
	verdict := "ok"
	worse := func(v string) {
		if verdictOrder[v] < verdictOrder[verdict] {
			verdict = v
		}
	}
	// At risk: use reaching a limit. Use can't go past a limit (memory is OOM-killed, CPU
	// throttled), so a peak close to the limit is the sign.
	if mem.Limit.Current >= 0 && mem.Max >= float64(mem.Limit.Current)*0.9 {
		add("bad", "Memory peaked at %s, %.0f%% of its %s limit: at the limit the container is OOM-killed.", fmtBytes(int64(mem.Max)), 100*mem.Max/float64(max(mem.Limit.Current, 1)), fmtBytes(mem.Limit.Current))
		worse("risk")
	}
	if cpu.Limit.Current >= 0 && cpu.Max >= float64(cpu.Limit.Current)*0.95 {
		add("warn", "CPU peaked at %s, at its %s limit: it gets throttled (slowed down) at its busiest.", fmtMilli(int64(cpu.Max)), fmtMilli(cpu.Limit.Current))
		worse("risk")
	}
	// Under-requested: typical use above the request but under the limit. Allowed (that's what
	// Burstable pods do), with side effects worth knowing.
	if mem.Request.Current >= 0 && float64(mem.Request.Current) < mem.Median {
		add("warn", "Typically uses %s of memory, above its %s request (fine while under the limit). If the node runs short of memory, pods using the most beyond their request are evicted first, and the scheduler counts only the request, so its node can end up fuller than Kubernetes thinks.", fmtBytes(int64(mem.Median)), fmtBytes(mem.Request.Current))
		worse("under")
	}
	if cpu.Request.Current >= 0 && float64(cpu.Request.Current) < cpu.Median {
		add("info", "Typically uses %s of CPU, above its %s request: fine, it uses spare CPU; when the node is busy it's held to roughly its request share.", fmtMilli(int64(cpu.Median)), fmtMilli(cpu.Request.Current))
	}
	if cpu.Request.Current < 0 || mem.Request.Current < 0 {
		add("warn", "No %s set: the scheduler doesn't reserve room for it.", missingWord(cpu.Request.Current < 0, mem.Request.Current < 0, "CPU request", "memory request"))
		worse("missing")
	}
	if cpu.Limit.Current < 0 || mem.Limit.Current < 0 {
		add("notice", "No %s set.", missingWord(cpu.Limit.Current < 0, mem.Limit.Current < 0, "CPU limit", "memory limit"))
		if mem.Limit.Current < 0 {
			worse("missing")
		}
	}
	if cpu.Request.Current > 0 && cpu.Request.Current >= 2*cpu.Request.Recommended && cpu.Request.Current-cpu.Request.Recommended >= 50 {
		add("info", "CPU request %s is %.0f× the suggested %s.", fmtMilli(cpu.Request.Current), float64(cpu.Request.Current)/float64(cpu.Request.Recommended), fmtMilli(cpu.Request.Recommended))
		worse("over")
	}
	if mem.Request.Current > 0 && mem.Request.Current >= 2*mem.Request.Recommended && mem.Request.Current-mem.Request.Recommended >= 64<<20 {
		add("info", "Memory request %s is %.0f× the suggested %s.", fmtBytes(mem.Request.Current), float64(mem.Request.Current)/float64(mem.Request.Recommended), fmtBytes(mem.Request.Recommended))
		worse("over")
	}
	if r.Days < optGoodDays {
		add("notice", "Based on %s of recorded data: better to wait for at least %.0f days before changing it.", fmtDays(r.Days), optGoodDays)
	}
	if r.SpanDays-r.Days >= 1 {
		add("info", "Gaps while k8s-buddy wasn't running are skipped (%s recorded over %s).", fmtDays(r.Days), fmtDays(r.SpanDays))
	}
	r.Findings, r.Verdict = f, verdict
	if f == nil {
		r.Findings = []OptFinding{}
	}
	r.FreeCPU, r.FreeMem = 0, 0
	if cpu.Request.Current >= 0 {
		r.FreeCPU = (cpu.Request.Current - cpu.Request.Recommended) * int64(r.Replicas)
	}
	if mem.Request.Current >= 0 {
		r.FreeMem = (mem.Request.Current - mem.Request.Recommended) * int64(r.Replicas)
	}
	r.Command = optCommand(r)
}

func missingWord(a, b bool, x, y string) string {
	switch {
	case a && b:
		return x + " or " + y
	case a:
		return x
	}
	return y
}

func optCommand(r *OptRow) string {
	res := fmt.Sprintf("--requests=cpu=%s,memory=%s --limits=cpu=%s,memory=%s",
		cpuQty(r.CPU.Request.Recommended), memQty(r.Mem.Request.Recommended), cpuQty(r.CPU.Limit.Recommended), memQty(r.Mem.Limit.Recommended))
	if r.PodLevel {
		return "# whole-pod figures: split them across its containers (per-container data is still being collected)\n# " + res
	}
	target := map[string]string{"Deployment": "deployment", "StatefulSet": "statefulset", "DaemonSet": "daemonset", "ReplicaSet": "replicaset", "Job": "job", "CronJob": "cronjob"}[r.Kind]
	if target == "" {
		return "# set in the pod spec of " + r.Kind + " " + r.Workload + ": " + res
	}
	cmd := kubectl(r.Cluster, r.Namespace, fmt.Sprintf("set resources %s/%s -c %s %s", target, r.Workload, r.Container, res))
	if r.Helm {
		cmd = "# managed by Helm: set these in the chart values instead, or the next upgrade reverts them\n" + cmd
	}
	return cmd
}

func roundCPU(m float64) int64 {
	step := 10.0
	switch {
	case m < 100:
		step = 5
	case m >= 1000:
		step = 50
	}
	return int64(math.Ceil(m/step) * step)
}

func roundMem(b float64) int64 {
	const mi = 1 << 20
	step := 16.0 * mi
	switch {
	case b >= 4<<30:
		step = 256 * mi
	case b >= 512*mi:
		step = 64 * mi
	}
	return int64(math.Ceil(b/step) * step)
}

func cpuQty(m int64) string {
	if m%1000 == 0 {
		return fmt.Sprint(m / 1000)
	}
	return fmt.Sprintf("%dm", m)
}

func memQty(b int64) string {
	if b%(1<<30) == 0 {
		return fmt.Sprintf("%dGi", b>>30)
	}
	return fmt.Sprintf("%dMi", b>>20)
}

func fmtMilli(m int64) string {
	if m >= 1000 {
		return fmt.Sprintf("%.2g cores", float64(m)/1000)
	}
	return fmt.Sprintf("%dm", m)
}

func fmtBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGi", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fMi", float64(b)/(1<<20))
	}
	return fmt.Sprintf("%dKi", b>>10)
}

func fmtDays(d float64) string {
	if d < 1 {
		return fmt.Sprintf("%.0f hours", math.Max(1, math.Round(d*24)))
	}
	return fmt.Sprintf("%.1f days", d)
}
