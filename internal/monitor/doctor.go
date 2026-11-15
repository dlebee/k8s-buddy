package monitor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/dlebee/k8s-buddy/internal/alert"
)

type DoctorCheck struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

var doctorChecks = []DoctorCheck{
	{"dangling-service", "Services with no pods", "Selector matches no running pods, so traffic goes nowhere."},
	{"ingress-backend", "Broken ingress backends", "Ingress points at a missing service, a missing port, or a service with no pods."},
	{"ingress-class", "Ingresses without a usable class", "Ingress names an IngressClass that does not exist, or names none and the cluster has no default."},
	{"nginx-mergeable", "NGINX mergeable ingresses", "Masters and minions NGINX ignores: minions without a master, masters without minions, and hosts claimed twice."},
	{"pdb-blocking", "PDBs blocking drains", "PodDisruptionBudgets that currently allow zero disruptions will stall node drains and upgrades."},
	{"hpa-target", "HPAs with missing targets", "Autoscaler targets a workload that does not exist."},
	{"orphaned-pv", "Released / failed volumes", "PersistentVolumes no claim uses anymore; their disks usually still cost money."},
	{"unused-pvc", "Unused volume claims", "Bound claims that no pod mounts."},
	{"leftover-pods", "Leftover failed pods", "Evicted or failed pods that were never cleaned up."},
	{"naked-pods", "Pods without a controller", "Pods not managed by a Deployment, Job, etc. won't come back if their node dies."},
}

type Finding struct {
	Cluster   string         `json:"cluster"`
	Check     string         `json:"check"`
	Severity  alert.Severity `json:"severity"`
	Kind      string         `json:"kind"`
	Namespace string         `json:"namespace,omitempty"`
	Name      string         `json:"name"`
	Message   string         `json:"message"`
	Hint      string         `json:"hint,omitempty"`
}

type DoctorReport struct {
	Running    bool              `json:"running"`
	StartedAt  time.Time         `json:"startedAt,omitzero"`
	FinishedAt time.Time         `json:"finishedAt,omitzero"`
	Clusters   []string          `json:"clusters"`
	Checks     []DoctorCheck     `json:"checks"`
	Findings   []Finding         `json:"findings"`
	Errors     map[string]string `json:"errors"`
}

type doctor struct {
	mu     sync.Mutex
	report DoctorReport
}

func (m *Manager) DoctorReport() DoctorReport {
	m.doc.mu.Lock()
	defer m.doc.mu.Unlock()
	r := m.doc.report
	r.Checks = doctorChecks
	if r.Findings == nil {
		r.Findings = []Finding{}
	}
	return r
}

// StartDoctor runs a one-off checkup across connected clusters in the background.
// It returns false if a checkup is already running.
func (m *Manager) StartDoctor() bool {
	m.doc.mu.Lock()
	if m.doc.report.Running {
		m.doc.mu.Unlock()
		return false
	}
	m.doc.report = DoctorReport{Running: true, StartedAt: time.Now(), Errors: map[string]string{}}
	m.doc.mu.Unlock()

	go func() {
		var clusters []*cluster
		for _, c := range m.connected() {
			if c.synced("pods") && c.synced("services") {
				clusters = append(clusters, c)
			}
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		var all []Finding
		errs := map[string]string{}
		names := make([]string, 0, len(clusters))
		for _, c := range clusters {
			names = append(names, c.name)
			wg.Go(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				defer cancel()
				f, problems := c.doctor(ctx)
				mu.Lock()
				all = append(all, f...)
				if len(problems) > 0 {
					errs[c.name] = strings.Join(problems, "; ")
				}
				mu.Unlock()
			})
		}
		wg.Wait()
		slices.SortStableFunc(all, func(a, b Finding) int {
			if r := b.Severity.Rank() - a.Severity.Rank(); r != 0 {
				return r
			}
			return strings.Compare(a.Cluster+a.Namespace+a.Name, b.Cluster+b.Namespace+b.Name)
		})
		m.doc.mu.Lock()
		m.doc.report.Running = false
		m.doc.report.FinishedAt = time.Now()
		m.doc.report.Findings = all
		m.doc.report.Errors = errs
		m.doc.report.Clusters = names
		m.doc.mu.Unlock()
	}()
	return true
}

func selectsAny(pods []*corev1.Pod, sel map[string]string) bool {
	s := labels.SelectorFromSet(sel)
	for _, p := range pods {
		if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed && s.Matches(labels.Set(p.Labels)) {
			return true
		}
	}
	return false
}

func (c *cluster) doctor(ctx context.Context) ([]Finding, []string) {
	var out []Finding
	var problems []string
	add := func(check string, sev alert.Severity, kind, ns, name, msg, hint string) {
		out = append(out, Finding{Cluster: c.name, Check: check, Severity: sev, Kind: kind, Namespace: ns, Name: name, Message: msg, Hint: hint})
	}

	pods, _ := c.pods.List(labels.Everything())
	podsByNS := map[string][]*corev1.Pod{}
	for _, p := range pods {
		podsByNS[p.Namespace] = append(podsByNS[p.Namespace], p)
	}
	svcs, _ := c.services.List(labels.Everything())
	svcIndex := map[string]*corev1.Service{}
	dangling := map[string]bool{}
	for _, s := range svcs {
		svcIndex[s.Namespace+"/"+s.Name] = s
		if s.Spec.Type == corev1.ServiceTypeExternalName || len(s.Spec.Selector) == 0 {
			continue
		}
		if !selectsAny(podsByNS[s.Namespace], s.Spec.Selector) {
			dangling[s.Namespace+"/"+s.Name] = true
			add("dangling-service", alert.Warning, "Service", s.Namespace, s.Name,
				fmt.Sprintf("selector %s matches no running pods", labels.SelectorFromSet(s.Spec.Selector)),
				kubectl(c.name, s.Namespace, "get pods -l "+shellQuote(labels.SelectorFromSet(s.Spec.Selector).String())))
		}
	}

	// Ingresses are listed on demand; we don't keep an informer for them.
	if ings, err := c.cs.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{}); err != nil {
		problems = append(problems, "ingresses: "+truncate(err.Error(), 120))
	} else {
		classes := map[string]bool{}
		hasDefault := false
		if ics, err := c.cs.NetworkingV1().IngressClasses().List(ctx, metav1.ListOptions{}); err == nil {
			for _, ic := range ics.Items {
				classes[ic.Name] = true
				if ic.Annotations[networkingv1.AnnotationIsDefaultIngressClass] == "true" {
					hasDefault = true
				}
			}
			for _, ing := range ings.Items {
				class := ingressClassOf(ing.Annotations, ing.Spec.IngressClassName)
				hint := kubectl(c.name, "", "get ingressclass")
				switch {
				case class != "" && !classes[class]:
					add("ingress-class", alert.Warning, "Ingress", ing.Namespace, ing.Name, fmt.Sprintf("uses ingress class %q which does not exist", class), hint)
				case class == "" && !hasDefault:
					add("ingress-class", alert.Warning, "Ingress", ing.Namespace, ing.Name, "has no ingress class and the cluster has no default class", hint)
				}
			}
		} else {
			problems = append(problems, "ingressclasses: "+truncate(err.Error(), 120))
		}
		ptrs := make([]*networkingv1.Ingress, len(ings.Items))
		for i := range ings.Items {
			ptrs[i] = &ings.Items[i]
		}
		merge := nginxMergeIssues(ptrs)
		for _, ing := range ptrs {
			if mi, ok := merge[ingressKey(ing)]; ok {
				add("nginx-mergeable", mi.Severity, "Ingress", ing.Namespace, ing.Name, mi.Message, kubectl(c.name, ing.Namespace, "describe ingress "+ing.Name))
			}
		}
		for _, ing := range ings.Items {
			var backends []*networkingv1.IngressServiceBackend
			if b := ing.Spec.DefaultBackend; b != nil && b.Service != nil {
				backends = append(backends, b.Service)
			}
			for _, r := range ing.Spec.Rules {
				if r.HTTP == nil {
					continue
				}
				for _, p := range r.HTTP.Paths {
					if p.Backend.Service != nil {
						backends = append(backends, p.Backend.Service)
					}
				}
			}
			seen := map[string]bool{}
			for _, b := range backends {
				key := ing.Namespace + "/" + b.Name
				if seen[key+fmt.Sprint(b.Port)] {
					continue
				}
				seen[key+fmt.Sprint(b.Port)] = true
				hint := kubectl(c.name, ing.Namespace, "describe ingress "+ing.Name)
				svc, ok := svcIndex[key]
				switch {
				case !ok:
					add("ingress-backend", alert.Warning, "Ingress", ing.Namespace, ing.Name, "points at missing service "+b.Name, hint)
				case !servicePortExists(svc, b.Port):
					add("ingress-backend", alert.Warning, "Ingress", ing.Namespace, ing.Name,
						fmt.Sprintf("service %s has no port %s", b.Name, portString(b.Port)), hint)
				case dangling[key]:
					add("ingress-backend", alert.Warning, "Ingress", ing.Namespace, ing.Name, "points at service "+b.Name+" which has no pods", hint)
				}
			}
		}
	}

	if pdbs, err := c.cs.PolicyV1().PodDisruptionBudgets("").List(ctx, metav1.ListOptions{}); err != nil {
		problems = append(problems, "pdbs: "+truncate(err.Error(), 120))
	} else {
		for _, p := range pdbs.Items {
			st := p.Status
			if st.DisruptionsAllowed > 0 || st.ExpectedPods == 0 {
				continue
			}
			msg := fmt.Sprintf("allows 0 disruptions (%d/%d healthy, %d required) — node drains will hang", st.CurrentHealthy, st.ExpectedPods, st.DesiredHealthy)
			if st.CurrentHealthy < st.DesiredHealthy {
				msg = fmt.Sprintf("allows 0 disruptions because pods are unhealthy (%d/%d healthy, %d required)", st.CurrentHealthy, st.ExpectedPods, st.DesiredHealthy)
			}
			add("pdb-blocking", alert.Warning, "PodDisruptionBudget", p.Namespace, p.Name, msg, kubectl(c.name, p.Namespace, "describe pdb "+p.Name))
		}
	}

	if hpas, err := c.cs.AutoscalingV2().HorizontalPodAutoscalers("").List(ctx, metav1.ListOptions{}); err != nil {
		problems = append(problems, "hpas: "+truncate(err.Error(), 120))
	} else {
		for _, h := range hpas.Items {
			ref := h.Spec.ScaleTargetRef
			var err error
			switch ref.Kind {
			case "Deployment":
				_, err = c.deploys.Deployments(h.Namespace).Get(ref.Name)
			case "StatefulSet":
				_, err = c.sts.StatefulSets(h.Namespace).Get(ref.Name)
			default:
				continue
			}
			if err != nil {
				add("hpa-target", alert.Warning, "HorizontalPodAutoscaler", h.Namespace, h.Name,
					fmt.Sprintf("targets %s/%s which does not exist", strings.ToLower(ref.Kind), ref.Name), kubectl(c.name, h.Namespace, "describe hpa "+h.Name))
			}
		}
	}

	if pvs, err := c.cs.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{}); err != nil {
		problems = append(problems, "pvs: "+truncate(err.Error(), 120))
	} else {
		for _, pv := range pvs.Items {
			if pv.Status.Phase != corev1.VolumeReleased && pv.Status.Phase != corev1.VolumeFailed {
				continue
			}
			size := ""
			if q, ok := pv.Spec.Capacity[corev1.ResourceStorage]; ok {
				size = q.String()
			}
			claim := ""
			if pv.Spec.ClaimRef != nil {
				claim = " · was " + pv.Spec.ClaimRef.Namespace + "/" + pv.Spec.ClaimRef.Name
			}
			add("orphaned-pv", alert.Notice, "PersistentVolume", "", pv.Name,
				fmt.Sprintf("%s %s · reclaim policy %s%s · %s old", pv.Status.Phase, size, pv.Spec.PersistentVolumeReclaimPolicy, claim, ago(pv.CreationTimestamp.Time)),
				kubectl(c.name, "", "describe pv "+pv.Name))
		}
	}

	if c.synced("pvcs") {
		mounted := map[string]bool{}
		for _, p := range pods {
			if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				continue
			}
			for _, v := range p.Spec.Volumes {
				if v.PersistentVolumeClaim != nil {
					mounted[p.Namespace+"/"+v.PersistentVolumeClaim.ClaimName] = true
				}
				if v.Ephemeral != nil {
					mounted[p.Namespace+"/"+p.Name+"-"+v.Name] = true
				}
			}
		}
		pvcs, _ := c.pvcs.List(labels.Everything())
		for _, pvc := range pvcs {
			if pvc.Status.Phase == corev1.ClaimBound && !mounted[pvc.Namespace+"/"+pvc.Name] {
				size := ""
				if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
					size = q.String() + " · "
				}
				add("unused-pvc", alert.Info, "PersistentVolumeClaim", pvc.Namespace, pvc.Name,
					size+"not mounted by any pod · "+ago(pvc.CreationTimestamp.Time)+" old", kubectl(c.name, pvc.Namespace, "describe pvc "+pvc.Name))
			}
		}
	}

	failed := map[string][]string{}
	for _, p := range pods {
		if p.Status.Phase == corev1.PodFailed {
			failed[p.Namespace] = append(failed[p.Namespace], p.Name)
		}
		if len(p.OwnerReferences) == 0 && p.Annotations["kubernetes.io/config.mirror"] == "" && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			add("naked-pods", alert.Info, "Pod", p.Namespace, p.Name, "no owning controller", kubectl(c.name, p.Namespace, "get pod "+p.Name+" -o yaml"))
		}
	}
	for ns, names := range failed {
		slices.Sort(names)
		add("leftover-pods", alert.Info, "Pod", ns, fmt.Sprintf("%d pod(s)", len(names)),
			truncate(strings.Join(names, ", "), 240),
			kubectl(c.name, ns, "delete pods --field-selector=status.phase=Failed"))
	}
	return out, problems
}

func servicePortExists(s *corev1.Service, p networkingv1.ServiceBackendPort) bool {
	for _, sp := range s.Spec.Ports {
		if (p.Name != "" && sp.Name == p.Name) || (p.Number != 0 && sp.Port == p.Number) {
			return true
		}
	}
	return p.Name == "" && p.Number == 0
}

func portString(p networkingv1.ServiceBackendPort) string {
	if p.Name != "" {
		return p.Name
	}
	return fmt.Sprint(p.Number)
}
