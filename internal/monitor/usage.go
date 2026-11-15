package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Requests, limits and live usage (from the metrics API, when the cluster serves it) for a pod or
// every pod of a workload. CPU is in millicores, memory in bytes; -1 means "not set".

type ResourceNumbers struct {
	CPU    int64 `json:"cpu"`
	Memory int64 `json:"memory"`
}

type ContainerUsage struct {
	Name     string           `json:"name"`
	Requests ResourceNumbers  `json:"requests"`
	Limits   ResourceNumbers  `json:"limits"`
	Usage    *ResourceNumbers `json:"usage,omitempty"`
}

type PodUsage struct {
	Name       string           `json:"name"`
	Health     Health           `json:"health"`
	Containers []ContainerUsage `json:"containers"`
}

type UsageReport struct {
	// Metrics is false when the cluster has no metrics API (metrics-server) or it failed.
	Metrics    bool       `json:"metrics"`
	MetricsErr string     `json:"metricsError,omitempty"`
	Window     string     `json:"window,omitempty"`
	Timestamp  time.Time  `json:"timestamp,omitzero"`
	Pods       []PodUsage `json:"pods"`
}

const maxUsagePods = 200

// podsOf returns the live pods behind a pod or a workload, using the same selection as the drawer.
func (m *Manager) podsOf(clusterName, kind, ns, name string) (*cluster, []*corev1.Pod, error) {
	c, err := m.cluster(clusterName)
	if err != nil {
		return nil, nil, err
	}
	if kind == "pods" {
		p, err := c.pods.Pods(ns).Get(name)
		if err != nil {
			return nil, nil, err
		}
		return c, []*corev1.Pod{p}, nil
	}
	d, err := m.Object(clusterName, kind, ns, name)
	if err != nil {
		return nil, nil, err
	}
	var out []*corev1.Pod
	for _, b := range d.Pods {
		if p, err := c.pods.Pods(ns).Get(b.Name); err == nil {
			out = append(out, p)
		}
	}
	return c, out, nil
}

func quantity(rl corev1.ResourceList, name corev1.ResourceName, milli bool) int64 {
	q, ok := rl[name]
	if !ok {
		return -1
	}
	if milli {
		return q.MilliValue()
	}
	return q.Value()
}

func (m *Manager) Usage(clusterName, kind, ns, name string) (UsageReport, error) {
	rep := UsageReport{Pods: []PodUsage{}}
	c, pods, err := m.podsOf(clusterName, kind, ns, name)
	if err != nil {
		return rep, err
	}
	if len(pods) > maxUsagePods {
		pods = pods[:maxUsagePods]
	}
	for _, p := range pods {
		_, h := podStatus(p)
		pu := PodUsage{Name: p.Name, Health: h, Containers: []ContainerUsage{}}
		for _, ct := range p.Spec.Containers {
			pu.Containers = append(pu.Containers, ContainerUsage{Name: ct.Name,
				Requests: ResourceNumbers{quantity(ct.Resources.Requests, corev1.ResourceCPU, true), quantity(ct.Resources.Requests, corev1.ResourceMemory, false)},
				Limits:   ResourceNumbers{quantity(ct.Resources.Limits, corev1.ResourceCPU, true), quantity(ct.Resources.Limits, corev1.ResourceMemory, false)}})
		}
		rep.Pods = append(rep.Pods, pu)
	}
	if len(pods) == 0 {
		return rep, nil
	}
	usage, window, ts, err := c.podMetrics(ns, pods)
	if err != nil {
		rep.MetricsErr = truncate(err.Error(), 200)
		return rep, nil
	}
	rep.Metrics, rep.Window, rep.Timestamp = true, window, ts
	for i := range rep.Pods {
		for j := range rep.Pods[i].Containers {
			if u, ok := usage[rep.Pods[i].Name+"/"+rep.Pods[i].Containers[j].Name]; ok {
				rep.Pods[i].Containers[j].Usage = &u
			}
		}
	}
	return rep, nil
}

type podMetricsList struct {
	Items []podMetricsItem `json:"items"`
}

type podMetricsItem struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Timestamp  time.Time `json:"timestamp"`
	Window     string    `json:"window"`
	Containers []struct {
		Name  string `json:"name"`
		Usage struct {
			CPU    string `json:"cpu"`
			Memory string `json:"memory"`
		} `json:"usage"`
	} `json:"containers"`
}

// podMetrics reads metrics.k8s.io for the given pods: one call per pod when there are few, else one
// namespace list (the metrics API ignores label selectors on some versions).
func (c *cluster) podMetrics(ns string, pods []*corev1.Pod) (map[string]ResourceNumbers, string, time.Time, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	base := "/apis/metrics.k8s.io/v1beta1/namespaces/" + ns + "/pods"
	var items []podMetricsItem
	if len(pods) <= 3 {
		for _, p := range pods {
			raw, err := c.cs.CoreV1().RESTClient().Get().AbsPath(base, p.Name).DoRaw(ctx)
			if err != nil {
				if len(pods) == 1 {
					return nil, "", time.Time{}, metricsErr(err)
				}
				continue // a pod that just started has no sample yet
			}
			var it podMetricsItem
			if err := json.Unmarshal(raw, &it); err == nil {
				items = append(items, it)
			}
		}
	} else {
		raw, err := c.cs.CoreV1().RESTClient().Get().AbsPath(base).DoRaw(ctx)
		if err != nil {
			return nil, "", time.Time{}, metricsErr(err)
		}
		var l podMetricsList
		if err := json.Unmarshal(raw, &l); err != nil {
			return nil, "", time.Time{}, err
		}
		names := map[string]bool{}
		for _, p := range pods {
			names[p.Name] = true
		}
		for _, it := range l.Items {
			if names[it.Metadata.Name] {
				items = append(items, it)
			}
		}
	}
	out := map[string]ResourceNumbers{}
	var window string
	var ts time.Time
	for _, it := range items {
		window = it.Window
		if it.Timestamp.After(ts) {
			ts = it.Timestamp
		}
		for _, ct := range it.Containers {
			cpu, _ := resource.ParseQuantity(firstNonEmpty(ct.Usage.CPU, "0"))
			mem, _ := resource.ParseQuantity(firstNonEmpty(ct.Usage.Memory, "0"))
			out[it.Metadata.Name+"/"+ct.Name] = ResourceNumbers{CPU: cpu.MilliValue(), Memory: mem.Value()}
		}
	}
	return out, window, ts, nil
}

func metricsErr(err error) error {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "could not find the requested resource") || strings.Contains(msg, "404") {
		return fmt.Errorf("metrics API not available on this cluster (metrics-server not installed?)")
	}
	return err
}
