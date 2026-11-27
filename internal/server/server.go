package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"os"
	"sigs.k8s.io/yaml"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dlebee/k8s-buddy/internal/alert"
	"github.com/dlebee/k8s-buddy/internal/config"
	"github.com/dlebee/k8s-buddy/internal/monitor"
	"github.com/dlebee/k8s-buddy/internal/update"
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
	updates  *update.Checker
	api      http.Handler // the routes without the token guard, for the MCP server's in-process reads
	mcp      *mcpServer

	codesMu sync.Mutex
	codes   map[string]time.Time // one-time page codes → expiry, see BrowserURL
}

// A code from BrowserURL must be redeemed this soon.
const codeTTL = 2 * time.Minute

func New(store *alert.Store, settings *config.Store, mgr *monitor.Manager) *Server {
	var b [24]byte
	_, _ = rand.Read(b[:])
	s := &Server{token: hex.EncodeToString(b[:]), store: store, settings: settings, mgr: mgr}
	s.forwards = &forwarder{env: s.kubectlEnv}
	return s
}

func (s *Server) SetActions(a Actions) { s.actions = a }

// SetUpdates connects the release checker (Settings → Updates, the buddy's update badge).
func (s *Server) SetUpdates(u *update.Checker) { s.updates = u }

// Listen binds a random loopback port.
func (s *Server) Listen() (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s.addr = ln.Addr().String()
	return ln, nil
}

// URL is a page URL carrying the API token, for the app's own web views (they keep no history).
func (s *Server) URL(page string) string {
	return "http://" + s.addr + "/" + page + "?t=" + s.token
}

// BrowserURL is a page URL for the user's default browser. Browsers keep visited URLs in their
// history (and extensions can read them), so it carries a one-time code that the page swaps for
// the token, instead of the token itself.
func (s *Server) BrowserURL(page string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	code := hex.EncodeToString(b[:])
	now := time.Now()
	s.codesMu.Lock()
	if s.codes == nil {
		s.codes = map[string]time.Time{}
	}
	for c, exp := range s.codes {
		if now.After(exp) {
			delete(s.codes, c)
		}
	}
	s.codes[code] = now.Add(codeTTL)
	s.codesMu.Unlock()
	return "http://" + s.addr + "/" + page + "?c=" + code
}

// redeem uses up a code from BrowserURL; it reports whether the code was valid.
func (s *Server) redeem(code string) bool {
	s.codesMu.Lock()
	defer s.codesMu.Unlock()
	exp, ok := s.codes[code]
	delete(s.codes, code)
	return ok && time.Now().Before(exp)
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

	// Swaps a one-time code from BrowserURL for the token. The only API that works without the token.
	mux.HandleFunc("POST /api/session", func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && o != "http://"+s.addr {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var req struct{ Code string }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil || req.Code == "" || !s.redeem(req.Code) {
			http.Error(w, "this link has expired: open k8s-buddy again", http.StatusForbidden)
			return
		}
		writeJSON(w, map[string]string{"token": s.token})
	})
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
		writeJSON(w, map[string]any{"enabled": true, "points": pts, "info": info, "bucketSeconds": 60,
			"retentionDays": s.settings.Get().UsageRetentionDays})
	})
	// Node-removal scenarios for the clusters carrying every picked tag (simulation only).
	mux.HandleFunc("GET /api/cost/scenarios", func(w http.ResponseWriter, r *http.Request) {
		if !s.settings.Get().BetaOn("cost") {
			http.Error(w, "Cost is a beta feature: turn it on in Settings → Beta features", http.StatusNotFound)
			return
		}
		writeJSON(w, s.mgr.CostScenarios(listParam(r, "tag"), listParam(r, "cluster"), listParam(r, "ns")))
	})
	// Re-reads the kubeconfig now rather than at the next 30-second check (Settings button).
	mux.HandleFunc("POST /api/kubeconfig/reload", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		res, err := s.mgr.ReloadNow(ctx)
		if err != nil {
			http.Error(w, "the kubeconfig is still loading: try again in a moment", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, res)
	})
	// The Quotas page asks for everything; the filters are for the MCP tool, whose reader can't
	// take 900 rows: namespace, verdict, limit (rows are already sorted most urgent first) and brief.
	mux.HandleFunc("GET /api/optimizer", func(w http.ResponseWriter, r *http.Request) {
		rep := s.mgr.Optimize(listParam(r, "cluster"))
		q := r.URL.Query()
		nss, verdicts := listParam(r, "namespace"), listParam(r, "verdict")
		limit, _ := strconv.Atoi(q.Get("limit"))
		brief := q.Get("brief") == "1"
		if len(nss) == 0 && len(verdicts) == 0 && limit <= 0 && !brief {
			writeJSON(w, rep)
			return
		}
		for _, v := range verdicts {
			if !monitor.ValidVerdict(v) {
				http.Error(w, fmt.Sprintf("unknown verdict %q: use risk, missing, under, over or ok", v), http.StatusBadRequest)
				return
			}
		}
		writeJSON(w, filterOpt(rep, nss, verdicts, limit, brief, q.Get("system") == "1"))
	})
	// Marks a workload's OOM kills as known and expected (or not any more). Kills after the mute
	// still count, so it comes back if they get frequent again.
	mux.HandleFunc("POST /api/optimizer/mute", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Cluster, Namespace, Kind, Workload string
			Mute                               bool
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		key, ok := config.OOMMuteKey(in.Cluster, in.Namespace, in.Kind, in.Workload)
		if !ok {
			http.Error(w, "bad workload", http.StatusBadRequest)
			return
		}
		err := s.settings.Update(func(st *config.Settings) {
			if !in.Mute {
				delete(st.MutedOOMs, key)
				return
			}
			if st.MutedOOMs == nil {
				st.MutedOOMs = map[string]time.Time{}
			}
			st.MutedOOMs[key] = time.Now().Truncate(time.Second)
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/spikes", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		days, _ := strconv.Atoi(q.Get("days"))
		pct := func(k string) float64 { v, _ := strconv.ParseFloat(q.Get(k), 64); return v }
		writeJSON(w, s.mgr.Spikes(listParam(r, "cluster"), days, monitor.SpikeLimits{
			NodeCPU: pct("nodeCpu"), NodeMem: pct("nodeMem"), PodCPU: pct("podCpu"), PodMem: pct("podMem")}))
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
		if q.Get("kind") == "nodes" {
			rep, err := s.mgr.NodeUsage(q.Get("cluster"), q.Get("name"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			writeJSON(w, rep)
			return
		}
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
		if in.MCP.Enabled && !config.ValidMCPKey(in.MCP.Key) {
			http.Error(w, fmt.Sprintf("The MCP key must be %d to %d letters, digits or symbols, without spaces.", config.MinMCPKey, config.MaxMCPKey), http.StatusBadRequest)
			return
		}
		if in.MCP.Port != 0 && (in.MCP.Port < 1024 || in.MCP.Port > 65535) {
			http.Error(w, "The MCP port must be between 1024 and 65535.", http.StatusBadRequest)
			return
		}
		// OOM mutes are changed from Quotas, not from the Settings page: keep the current ones.
		if err := s.settings.Update(func(st *config.Settings) { in.MutedOOMs = st.MutedOOMs; *st = in }); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, s.settings.Get())
	})
	mux.HandleFunc("GET /api/update", func(w http.ResponseWriter, r *http.Request) {
		if s.updates == nil {
			writeJSON(w, update.Status{})
			return
		}
		writeJSON(w, s.updates.Status())
	})
	mux.HandleFunc("POST /api/update/check", func(w http.ResponseWriter, r *http.Request) {
		if s.updates != nil {
			s.updates.CheckNow()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// Opens the download or the release notes in the user's browser. The page only says which:
	// the link itself is the one the checker found on GitHub.
	mux.HandleFunc("POST /api/update/open", func(w http.ResponseWriter, r *http.Request) {
		if s.updates == nil {
			http.NotFound(w, r)
			return
		}
		st := s.updates.Status()
		url := st.Notes
		if r.URL.Query().Get("what") == "download" && st.Download != "" {
			url = st.Download
		}
		if url == "" {
			http.NotFound(w, r)
			return
		}
		if err := openURL(url); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
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
	mux.HandleFunc("GET /api/mcp", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.MCPStatus()) })
	s.api = mux
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
		// /api/session checks its own one-time code.
		public := strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/overview" || r.URL.Path == "/panel" || r.URL.Path == "/character" ||
			(r.URL.Path == "/api/session" && r.Method == http.MethodPost)
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

// filterOpt narrows a Quotas report (rows arrive sorted most urgent first) and counts what it left out.
// System workloads (kube-system and add-on namespaces) are left out unless asked for: their owner
// undoes manual changes, so their suggestions aren't something to act on.
func filterOpt(rep monitor.OptReport, nss, verdicts []string, limit int, brief, system bool) any {
	all := rep.Rows
	rep.Rows = slices.DeleteFunc(slices.Clone(all), func(x monitor.OptRow) bool { return x.System && !system })
	hidden := len(all) - len(rep.Rows)
	rows := slices.DeleteFunc(slices.Clone(rep.Rows), func(x monitor.OptRow) bool {
		return len(nss) > 0 && !slices.Contains(nss, x.Namespace) || len(verdicts) > 0 && !slices.Contains(verdicts, x.Verdict)
	})
	out := optAnswer{OptReport: rep, Total: len(rep.Rows), Matching: len(rows), ByVerdict: map[string]int{}, SystemHidden: hidden,
		OmittedByVerdict: map[string]int{}, Warnings: []string{}}
	for _, x := range rep.Rows {
		if len(nss) == 0 || slices.Contains(nss, x.Namespace) {
			out.ByVerdict[x.Verdict]++
		}
	}
	// A namespace that matches nothing is more likely a typo than an empty namespace: say so
	// rather than answer "0 rows", which reads as "nothing to fix".
	for _, ns := range nss {
		inAll := slices.ContainsFunc(all, func(x monitor.OptRow) bool { return x.Namespace == ns })
		switch {
		case !inAll:
			out.Warnings = append(out.Warnings, fmt.Sprintf("namespace %q: no workload with recorded usage found in the selected clusters (check the spelling)", ns))
		case !slices.ContainsFunc(rep.Rows, func(x monitor.OptRow) bool { return x.Namespace == ns }):
			out.Warnings = append(out.Warnings, fmt.Sprintf("namespace %q holds only system workloads: pass system=true to see them", ns))
		}
	}
	if limit > 0 && len(rows) > limit {
		out.Truncated, out.Omitted = true, len(rows)-limit
		kept := spreadLimit(rows, limit)
		for _, x := range rows {
			out.OmittedByVerdict[x.Verdict]++
		}
		for _, x := range kept {
			out.OmittedByVerdict[x.Verdict]--
		}
		maps.DeleteFunc(out.OmittedByVerdict, func(_ string, n int) bool { return n == 0 })
		rows = kept
	}
	out.OptReport.Rows = rows
	if !brief {
		return out
	}
	out.OptReport.Rows = nil
	b := optBriefAnswer{optAnswer: out, Rows: make([]optBriefRow, len(rows))}
	for i, x := range rows {
		b.Rows[i] = optBriefRow{x.Cluster, x.Namespace, x.Kind, x.Workload, x.Container, x.Replicas, x.Verdict, x.Confidence, x.Days,
			x.CPU, x.Mem, x.OOMs, x.OOMBurst, x.System, x.Helm, x.Command}
	}
	return b
}

// spreadLimit keeps limit rows of rows (sorted most urgent first) without letting the urgent
// verdicts crowd out the others entirely: each verdict present first gets a small share (an
// eighth of the limit, at least one), then the rest goes to the most urgent. Order is kept.
func spreadLimit(rows []monitor.OptRow, limit int) []monitor.OptRow {
	share := max(1, limit/8)
	taken := map[string]int{}
	keep := make([]bool, len(rows))
	n := 0
	for i, x := range rows {
		if n < limit && taken[x.Verdict] < share {
			keep[i], n = true, n+1
			taken[x.Verdict]++
		}
	}
	for i := range rows {
		if n < limit && !keep[i] {
			keep[i], n = true, n+1
		}
	}
	out := make([]monitor.OptRow, 0, limit)
	for i, x := range rows {
		if keep[i] {
			out = append(out, x)
		}
	}
	return out
}

// optAnswer is a filtered Quotas report: how many rows there are in all, how many matched, how
// many of each verdict (in the picked clusters and namespaces), and whether a limit cut it short.
type optAnswer struct {
	monitor.OptReport
	Total     int            `json:"total"`
	Matching  int            `json:"matching"`
	ByVerdict map[string]int `json:"byVerdict"`
	Truncated bool           `json:"truncated"`
	Omitted   int            `json:"omitted"` // matching rows left out by the limit
	// SystemHidden counts system-workload rows left out (ask with system=1 to see them).
	SystemHidden int `json:"systemHidden"`
	// OmittedByVerdict says which verdicts the limit cut into; Warnings flags filters that matched nothing.
	OmittedByVerdict map[string]int `json:"omittedByVerdict"`
	Warnings         []string       `json:"warnings"`
}

// optBriefRow is a recommendation without its prose and chart pointers: the numbers and the command.
type optBriefRow struct {
	Cluster    string              `json:"cluster"`
	Namespace  string              `json:"namespace"`
	Kind       string              `json:"kind"`
	Workload   string              `json:"workload"`
	Container  string              `json:"container"`
	Replicas   int                 `json:"replicas"`
	Verdict    string              `json:"verdict"`
	Confidence string              `json:"confidence"`
	Days       float64             `json:"days"`
	CPU        monitor.OptResource `json:"cpu"`
	Mem        monitor.OptResource `json:"mem"`
	OOMs       int                 `json:"ooms"`
	OOMBurst   int                 `json:"oomBurst"`
	System     bool                `json:"system"` // managed by the cluster provider or an add-on: don't apply
	Helm       bool                `json:"helm"`   // managed by Helm: change the chart values instead
	Command    string              `json:"command"`
}

type optBriefAnswer struct {
	optAnswer
	Rows []optBriefRow `json:"rows"`
}

type summary struct {
	alert.Summary
	NudgeLevel string        `json:"nudgeLevel"`
	ShowBubble bool          `json:"showBubble"`
	Latest     []alert.Alert `json:"latest"`
	Doctor     doctorSummary `json:"doctor"`
	Update     updateSummary `json:"update"`
	Version    string        `json:"version"` // this build, shown in the panel and the overview
}

// updateSummary says whether a newer release is out, for the buddy's update badge.
type updateSummary struct {
	Available bool   `json:"available"`
	Latest    string `json:"latest,omitempty"`
}

// doctorSummary is the latest Doctor checkup, for the buddy's doctor badge and the panel.
type doctorSummary struct {
	Ran      bool      `json:"ran"`
	Running  bool      `json:"running"`
	At       time.Time `json:"at,omitzero"`     // when the latest checkup finished
	NextAt   time.Time `json:"nextAt,omitzero"` // next scheduled one (zero: schedule off)
	Findings int       `json:"findings"`
	Critical int       `json:"critical"`
	Warning  int       `json:"warning"`
	Clusters int       `json:"clusters"` // clusters with findings
}

func (s *Server) summary() summary {
	st := s.settings.Get()
	return summary{
		Summary:    s.store.Summary(),
		NudgeLevel: st.NudgeLevel,
		ShowBubble: st.ShowBubble,
		Latest:     s.store.List(alert.Query{Limit: 3}),
		Doctor:     s.doctorSummary(),
		Update:     s.updateSummary(),
		Version:    s.version(),
	}
}

func (s *Server) version() string {
	if s.updates == nil {
		return ""
	}
	return s.updates.Status().Current
}

func (s *Server) updateSummary() updateSummary {
	if s.updates == nil {
		return updateSummary{}
	}
	st := s.updates.Status()
	return updateSummary{Available: st.Available, Latest: st.Latest}
}

func (s *Server) doctorSummary() doctorSummary {
	r := s.mgr.LastDoctor()
	d := doctorSummary{Ran: !r.FinishedAt.IsZero(), Running: r.Running, At: r.FinishedAt, NextAt: r.NextAt, Findings: len(r.Findings)}
	clusters := map[string]bool{}
	for _, f := range r.Findings {
		clusters[f.Cluster] = true
		switch f.Severity {
		case alert.Critical:
			d.Critical++
		case alert.Warning:
			d.Warning++
		}
	}
	d.Clusters = len(clusters)
	return d
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
