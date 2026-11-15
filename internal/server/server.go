package server

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

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
}

func New(store *alert.Store, settings *config.Store, mgr *monitor.Manager) *Server {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return &Server{token: hex.EncodeToString(b[:]), store: store, settings: settings, mgr: mgr}
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
	files := http.FileServerFS(static)
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
			Cluster: r.URL.Query().Get("cluster"),
			Scope:   r.URL.Query().Get("scope"),
			Limit:   limit,
		}))
	})
	mux.HandleFunc("POST /api/alerts/{id}/ack", func(w http.ResponseWriter, r *http.Request) {
		s.store.Ack(r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/alerts/ack-all", func(w http.ResponseWriter, r *http.Request) {
		s.store.AckAll(r.URL.Query().Get("cluster"))
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
	mux.HandleFunc("GET /api/kinds", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, monitor.Kinds()) })
	mux.HandleFunc("GET /api/resources", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 || limit > 5000 {
			limit = 1500
		}
		list, err := s.mgr.Resources(monitor.ResourceQuery{
			Kind: q.Get("kind"), Cluster: q.Get("cluster"), Namespace: q.Get("namespace"),
			Search: q.Get("q"), Problems: q.Get("problems") == "1", Limit: limit,
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
	mux.HandleFunc("POST /api/exec", s.handleExec)
	mux.HandleFunc("GET /api/crds", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.mgr.CustomResources()) })
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
		isStatic := strings.HasPrefix(r.URL.Path, "/static/")
		if !isStatic && subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
