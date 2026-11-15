package monitor

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

var cronJobSuffix = regexp.MustCompile(`-\d{8,}$`)

// workloadOf maps a pod to its top-level controller so alerts dedupe per workload, not per pod.
func workloadOf(p *corev1.Pod) (kind, name string) {
	for _, o := range p.OwnerReferences {
		if o.Controller == nil || !*o.Controller {
			continue
		}
		switch o.Kind {
		case "ReplicaSet":
			if h := p.Labels["pod-template-hash"]; h != "" && strings.HasSuffix(o.Name, "-"+h) {
				return "Deployment", strings.TrimSuffix(o.Name, "-"+h)
			}
			return o.Kind, o.Name
		case "Job":
			return jobOwner(o.Name)
		default:
			return o.Kind, o.Name
		}
	}
	return "Pod", p.Name
}

func jobOwner(job string) (string, string) {
	if cronJobSuffix.MatchString(job) {
		return "CronJob", cronJobSuffix.ReplaceAllString(job, "")
	}
	return "Job", job
}

var shortKinds = map[string]string{
	"Deployment": "deploy", "StatefulSet": "sts", "DaemonSet": "ds", "ReplicaSet": "rs",
	"Job": "job", "CronJob": "cj", "Pod": "pod", "Node": "node", "Service": "svc",
	"PersistentVolumeClaim": "pvc", "PersistentVolume": "pv",
}

func shortRef(kind, name string) string {
	if s, ok := shortKinds[kind]; ok {
		return s + "/" + name
	}
	return strings.ToLower(kind) + "/" + name
}

var poolLabels = []string{
	"cloud.google.com/gke-nodepool",
	"eks.amazonaws.com/nodegroup",
	"kubernetes.azure.com/agentpool",
	"agentpool",
	"karpenter.sh/nodepool",
	"doks.digitalocean.com/node-pool",
	"node.kubernetes.io/pool",
	"nodepool",
}

func poolOf(n *corev1.Node) string {
	for _, l := range poolLabels {
		if v := n.Labels[l]; v != "" {
			return v
		}
	}
	if n.Labels["kubernetes.io/os"] == "windows" {
		return "windows"
	}
	return "default"
}

func nodeCondition(n *corev1.Node, t corev1.NodeConditionType) *corev1.NodeCondition {
	for i := range n.Status.Conditions {
		if n.Status.Conditions[i].Type == t {
			return &n.Status.Conditions[i]
		}
	}
	return nil
}

func kubectl(context, namespace, args string) string {
	s := "kubectl --context " + shellQuote(context)
	if namespace != "" {
		s += " -n " + shellQuote(namespace)
	}
	return s + " " + args
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:@", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func joinNonEmpty(sep string, parts ...string) string {
	out := parts[:0:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" && p != "container" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

func ago(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
