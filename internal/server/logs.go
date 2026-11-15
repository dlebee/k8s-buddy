package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/dlebee/k8s-buddy/internal/monitor"
)

var logKinds = map[string]bool{"pods": true, "deployments": true, "statefulsets": true, "daemonsets": true, "jobs": true}

// handleLogs streams container logs as server-sent events: one "meta", then "line" events, an "end"
// per finished container stream and a final "done". Writes are batched and flushed a few times a
// second so a chatty container can't turn every line into a network round trip.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	kind := q.Get("kind")
	if !logKinds[kind] {
		http.Error(w, "logs are only available for pods and workloads", http.StatusBadRequest)
		return
	}
	tail, _ := strconv.ParseInt(q.Get("tail"), 10, 64)
	opts := monitor.LogOptions{Container: q.Get("container"), Pod: q.Get("pod"), Tail: tail,
		Follow: q.Get("follow") == "1", Previous: q.Get("previous") == "1"}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var mu sync.Mutex
	started := false
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		_, _ = w.Write([]byte("event: " + event + "\ndata: "))
		_, _ = w.Write(b)
		_, _ = w.Write([]byte("\n\n"))
	}
	start := func() {
		if started {
			return
		}
		started = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}
	go func() { // flush batches and keep the connection alive
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		idle := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				mu.Lock()
				if started {
					if idle++; idle >= 100 { // ~20s of quiet
						_, _ = w.Write([]byte(": ping\n\n"))
						idle = 0
					}
					fl.Flush()
				}
				mu.Unlock()
			}
		}
	}()
	err := s.mgr.StreamLogs(ctx, q.Get("cluster"), kind, q.Get("namespace"), q.Get("name"), opts,
		func(m monitor.LogMeta) { mu.Lock(); start(); send("meta", m); fl.Flush(); mu.Unlock() },
		func(l monitor.LogLine) { mu.Lock(); send("line", l); mu.Unlock() },
		func(st monitor.LogStream, err error) {
			mu.Lock()
			ev := map[string]string{"pod": st.Pod, "container": st.Container}
			if err != nil {
				ev["error"] = err.Error()
			}
			send("end", ev)
			mu.Unlock()
		})
	mu.Lock()
	defer mu.Unlock()
	if err != nil && !started {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		send("failed", map[string]string{"error": err.Error()})
	}
	send("done", map[string]bool{"done": true})
	fl.Flush()
}
