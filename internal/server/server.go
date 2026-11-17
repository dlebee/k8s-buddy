package server

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"github.com/dlebee/k8s-buddy/internal/history"
	"io/fs"
	"net"
	"net/http"
	"os"
	"sigs.k8s.io/yaml"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dlebee/k8s-buddy/internal/alert"
	"github.com/dlebee/k8s-buddy/internal/config"
	"github.com/dlebee/k8s-buddy/internal/monitor"
)

//go:embed web
var webFS embed.FS

// Actions are native UI operations the web pages can trigger.
type Actions interface {
	ShowOverview(tab string)
	TogglePanel()
	HidePanel()
}

type Server struct {
	token    string
	addr     string
	store    *alert.Store
	settings *config.Store
	mgr      *monitor.Manager
	actions  Actions
	forwards *forwarder
}

func New(store *alert.Store, settings *config.Store, mgr *monitor.Manager) *Server {
	var b [24]byte
	_, _ = rand.Read(b[:])
	s := &Server{token: hex.EncodeToString(b[:]), store: store, settings: settings, mgr: mgr}
	s.forwards = &forwarder{env: s.kubectlEnv}
	return s
}

func (s *Server) SetActions(a Actions) { s.actions = a }

// Listen binds a random loopback port.
func (s *Server) Listen() (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s.addr = ln.Addr().String()
	return ln, nil
}

func (s *Server) URL(page string) string {
	return "http://" + s.addr + "/" + page + "?t=" + s.token
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	// Dev mode: K8S_BUDDY_WEB_DIR serves the web UI from disk, uncached, and pages reload when it changes.
	devDir := os.Getenv("K8S_BUDDY_WEB_DIR")
	if devDir != "" {
		static = os.DirFS(devDir)
	}
	var files http.Handler = http.FileServerFS(static)
	if devDir != "" {
		fsrv := files
		files = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			fsrv.ServeHTTP(w, r)
		})
		mux.HandleFunc("GET /api/dev/version", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, webVersion(static)) })
	}
	for _, p := range []string{"character", "panel", "overview"} {
		mux.HandleFunc("GET /"+p, func(w http.ResponseWriter, r *http.Request) {
			http.ServeFileFS(w, r, static, p+".html")
		})
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", files))

	mux.HandleFunc("GET /api/summary", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.summary()) })
	mux.HandleFunc("GET /api/alerts", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		writeJSON(w, s.store.List(alert.Query{
			Clusters: listParam(r, "cluster"),
			Scope:    r.URL.Query().Get("scope"),
			Limit:    limit,
		}))
	})
	mux.HandleFunc("POST /api/alerts/{id}/ack", func(w http.ResponseWriter, r *http.Request) {
		s.store.Ack(r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/alerts/ack-all", func(w http.ResponseWriter, r *http.Request) {
		s.store.AckAll(listParam(r, "cluster"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/alerts/{id}/mute", func(w http.ResponseWriter, r *http.Request) {
		a, ok := s.store.Get(r.PathValue("id"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		if err := s.settings.Update(func(st *config.Settings) { st.MutedKeys = append(st.MutedKeys, a.FullKey()) }); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/clusters", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.mgr.Overview()) })
	mux.HandleFunc("GET /api/exposure", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.mgr.Exposure(listParam(r, "cluster"))) })
	mux.HandleFunc("GET /api/kinds", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, monitor.Kinds()) })
	mux.HandleFunc("GET /api/resources", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 || limit > 5000 {
			limit = 1500
		}
		list, err := s.mgr.Resources(monitor.ResourceQuery{
			Kind: q.Get("kind"), Clusters: listParam(r, "cluster"), Namespaces: listParam(r, "namespace"),
			Search: q.Get("q"), Problems: q.Get("problems") == "1", Limit: limit, Sort: q.Get("sort"),
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, list)
	})
	mux.HandleFunc("GET /api/resource", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		d, err := s.mgr.Resource(q.Get("cluster"), q.Get("kind"), q.Get("namespace"), q.Get("name"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, d)
	})
	mux.HandleFunc("GET /api/object", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		d, err := s.mgr.Object(q.Get("cluster"), q.Get("kind"), q.Get("namespace"), q.Get("name"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, d)
	})
	mux.HandleFunc("POST /api/exec", s.handleExec)
	mux.HandleFunc("GET /api/ingress-routes", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		hosts, err := s.mgr.IngressRoutes(q.Get("cluster"), q.Get("namespace"), q.Get("name"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, hosts)
	})
	// Opens an ingress route in the default browser; the URL is rebuilt from the ingress itself.
	mux.HandleFunc("POST /api/ingress-open", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Cluster, Namespace, Name, Host, Path string }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		u, err := s.mgr.IngressURL(req.Cluster, req.Namespace, req.Name, req.Host, req.Path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := openURL(u); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/azure", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.mgr.AzureStatus()) })
	mux.HandleFunc("POST /api/azure/login", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.mgr.StartAzureLogin()) })
	mux.HandleFunc("GET /api/portforwards", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.forwards.snapshot()) })
	mux.HandleFunc("POST /api/portforwards", s.handleForwardStart)
	mux.HandleFunc("DELETE /api/portforwards/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !s.forwards.stop(r.PathValue("id")) {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/portforwards/{id}/open", s.handleForwardOpen)
	mux.HandleFunc("POST /api/shell", s.handleShell)
	mux.HandleFunc("GET /api/crds", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.mgr.CustomResources()) })
	// Reveals one key of one Secret on an explicit click; POST so it is never a link or a cached GET.
	mux.HandleFunc("POST /api/secret/reveal", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Cluster, Namespace, Name, Key string }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		v, err := s.mgr.RevealSecretKey(req.Cluster, req.Namespace, req.Name, req.Key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		out := map[string]any{"key": req.Key, "size": len(v)}
		if utf8.Valid(v) {
			out["text"] = string(v)
		} else {
			out["base64"] = base64.StdEncoding.EncodeToString(v)
		}
		writeJSON(w, out)
	})
	// Reads a Helm release's user-supplied values on an explicit click (never in the background).
	mux.HandleFunc("POST /api/helm/values", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Cluster, Namespace, Name string
			Revision                 int
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		hv, err := s.mgr.HelmReleaseValues(req.Cluster, req.Namespace, req.Name, req.Revision)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		y, err := yaml.Marshal(hv.Values)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"revision": hv.Revision, "status": hv.Status, "chart": hv.Chart, "chartVersion": hv.ChartVersion,
			"appVersion": hv.AppVersion, "empty": len(hv.Values) == 0, "yaml": string(y)})
	})
	mux.HandleFunc("GET /api/k8s-releases", func(w http.ResponseWriter, r *http.Request) {
		rel, at := monitor.K8sReleases()
		writeJSON(w, map[string]any{"releases": rel, "fetchedAt": at})
	})
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	// Usage over time from the local history: kind is node, pod, or workload (name "Kind/name").
	mux.HandleFunc("GET /api/usage/history", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		h := s.mgr.History()
		if h == nil || !s.settings.Get().UsageHistory {
			writeJSON(w, map[string]any{"enabled": false, "points": []any{}})
			return
		}
		hours, _ := strconv.Atoi(q.Get("hours"))
		if hours <= 0 {
			hours = 24
		}
		pts, info, err := h.Query(q.Get("cluster"), q.Get("kind"), q.Get("namespace"), q.Get("name"), time.Now().Add(-time.Duration(hours)*time.Hour))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"enabled": true, "points": pts, "info": info, "bucketSeconds": int(history.Bucket.Seconds()),
			"retentionDays": s.settings.Get().UsageRetentionDays})
	})
	mux.HandleFunc("GET /api/cost", func(w http.ResponseWriter, r *http.Request) {
		hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
		if hours <= 0 {
			hours = s.settings.Get().UsageRetentionDays * 24
		}
		writeJSON(w, s.mgr.CostReport(hours))
	})
	mux.HandleFunc("GET /api/usage/stats", func(w http.ResponseWriter, r *http.Request) {
		h := s.mgr.History()
		if h == nil {
			writeJSON(w, map[string]any{"available": false})
			return
		}
		series, samples, oldest, err := h.Stats()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"available": true, "series": series, "samples": samples, "oldest": oldest, "bytes": h.Size()})
	})
	mux.HandleFunc("GET /api/usage", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		rep, err := s.mgr.Usage(q.Get("cluster"), q.Get("kind"), q.Get("namespace"), q.Get("name"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, rep)
	})
	mux.HandleFunc("GET /api/describe", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		out, err := s.mgr.Describe(q.Get("cluster"), q.Get("kind"), q.Get("namespace"), q.Get("name"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(out))
	})
	mux.HandleFunc("GET /api/doctor", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.mgr.DoctorReport()) })
	mux.HandleFunc("POST /api/doctor", func(w http.ResponseWriter, r *http.Request) {
		s.mgr.StartDoctor()
		writeJSON(w, s.mgr.DoctorReport())
	})
	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.settings.Get()) })
	mux.HandleFunc("PUT /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var in config.Settings
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.settings.Update(func(st *config.Settings) { *st = in }); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, s.settings.Get())
	})
	mux.HandleFunc("POST /api/action/{name}", func(w http.ResponseWriter, r *http.Request) {
		if s.actions != nil {
			switch r.PathValue("name") {
			case "overview":
				s.actions.ShowOverview(r.URL.Query().Get("tab"))
			case "panel":
				s.actions.TogglePanel()
			case "hide-panel":
				s.actions.HidePanel()
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/stream", s.stream)
	return s.guard(mux)
}

// guard rejects requests that do not carry the session token or target another host
// (blocks DNS rebinding and drive-by requests from browser tabs).
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != s.addr {
			http.Error(w, "bad host", http.StatusForbidden)
			return
		}
		tok := r.URL.Query().Get("t")
		if tok == "" {
			tok = r.Header.Get("X-Buddy-Token")
		}
		// Static files and the three page shells hold no data, so they load without the token (the
		// pages drop it from the address bar and keep it in session storage); every API needs it.
		public := strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/overview" || r.URL.Path == "/panel" || r.URL.Path == "/character"
		if !public && subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		// Only our own scripts run: a missed escape somewhere can't turn into script execution.
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data:; connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

type summary struct {
	alert.Summary
	NudgeLevel string        `json:"nudgeLevel"`
	ShowBubble bool          `json:"showBubble"`
	Latest     []alert.Alert `json:"latest"`
}

func (s *Server) summary() summary {
	st := s.settings.Get()
	return summary{
		Summary:    s.store.Summary(),
		NudgeLevel: st.NudgeLevel,
		ShowBubble: st.ShowBubble,
		Latest:     s.store.List(alert.Query{Limit: 3}),
	}
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	ch, cancel := s.store.Subscribe()
	defer cancel()

	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		_, _ = w.Write([]byte("event: " + event + "\ndata: "))
		_, _ = w.Write(b)
		_, _ = w.Write([]byte("\n\n"))
		fl.Flush()
	}
	send("summary", s.summary())

	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if ev.Type == alert.EventNew {
				send("alert", ev.Alert)
			}
			debounce.Reset(400 * time.Millisecond)
		case <-debounce.C:
			send("summary", s.summary())
		case <-keepalive.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			fl.Flush()
		}
	}
}

// listParam reads a multi-value query parameter given as repeats and/or comma-separated values.
func listParam(r *http.Request, key string) []string {
	var out []string
	for _, v := range r.URL.Query()[key] {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// webVersion changes whenever a file under the dev web directory is modified, added or removed.
func webVersion(fsys fs.FS) string {
	var latest time.Time
	n := 0
	_ = fs.WalkDir(fsys, ".", func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(latest) {
			latest = info.ModTime()
		}
		n++
		return nil
	})
	return strconv.FormatInt(latest.UnixNano(), 10) + "-" + strconv.Itoa(n)
}
