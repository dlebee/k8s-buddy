package monitor

import (
	"bufio"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
)

// Log streaming for a pod, or for every pod of a workload merged into one stream.

type LogOptions struct {
	Container string // "" = every container
	Pod       string // workloads: just this pod
	Tail      int64
	Follow    bool
	Previous  bool
}

type LogLine struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Time      string `json:"ts,omitempty"`
	Text      string `json:"text"`
}

type LogStream struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
}

type LogMeta struct {
	Streams   []LogStream `json:"streams"`
	Pods      []string    `json:"pods"`       // every pod behind the object
	Omitted   int         `json:"omitted"`    // pods left out over the cap
	Container []string    `json:"containers"` // containers of the (first) pod template
}

const (
	maxLogStreams = 40
	maxLogTail    = 5000
	maxLogLine    = 64 << 10
)

// StreamLogs calls meta once, then line for every log line (serialized), then end per stream that
// finishes, until ctx is cancelled or every stream ends.
func (m *Manager) StreamLogs(ctx context.Context, clusterName, kind, ns, name string, o LogOptions,
	meta func(LogMeta), line func(LogLine), end func(LogStream, error)) error {
	c, pods, err := m.podsOf(clusterName, kind, ns, name)
	if err != nil {
		return err
	}
	slices.SortFunc(pods, func(a, b *corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
	md := LogMeta{Pods: []string{}, Container: []string{}, Streams: []LogStream{}}
	for _, p := range pods {
		md.Pods = append(md.Pods, p.Name)
	}
	if len(pods) > 0 {
		for _, ct := range pods[0].Spec.Containers {
			md.Container = append(md.Container, ct.Name)
		}
	}
	if o.Pod != "" {
		pods = slices.DeleteFunc(pods, func(p *corev1.Pod) bool { return p.Name != o.Pod })
		if len(pods) == 0 {
			return fmt.Errorf("pod %s is not part of %s", o.Pod, name)
		}
	}
	if o.Tail <= 0 || o.Tail > maxLogTail {
		o.Tail = min(max(o.Tail, 200), maxLogTail)
	}
	for _, p := range pods {
		for _, ct := range p.Spec.Containers {
			if o.Container != "" && ct.Name != o.Container {
				continue
			}
			if len(md.Streams) >= maxLogStreams {
				md.Omitted++
				break
			}
			md.Streams = append(md.Streams, LogStream{Pod: p.Name, Container: ct.Name})
		}
	}
	if o.Container != "" && len(md.Streams) == 0 {
		return fmt.Errorf("no container %q", o.Container)
	}
	meta(md)

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, s := range md.Streams {
		wg.Go(func() {
			err := c.streamOne(ctx, ns, s, o, func(l LogLine) { mu.Lock(); line(l); mu.Unlock() })
			mu.Lock()
			end(s, err)
			mu.Unlock()
		})
	}
	wg.Wait()
	return nil
}

func (c *cluster) streamOne(ctx context.Context, ns string, s LogStream, o LogOptions, emit func(LogLine)) error {
	tail := o.Tail
	req := c.cs.CoreV1().Pods(ns).GetLogs(s.Pod, &corev1.PodLogOptions{
		Container: s.Container, Follow: o.Follow, Previous: o.Previous, TailLines: &tail, Timestamps: true,
	})
	rc, err := req.Stream(ctx)
	if err != nil {
		return err
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64<<10), maxLogLine)
	for sc.Scan() {
		t := sc.Text()
		l := LogLine{Pod: s.Pod, Container: s.Container, Text: t}
		if ts, rest, ok := strings.Cut(t, " "); ok && len(ts) >= 20 && ts[4] == '-' && ts[10] == 'T' {
			l.Time, l.Text = ts, rest
		}
		emit(l)
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
