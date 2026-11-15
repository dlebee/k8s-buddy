package server

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Port forwards run `kubectl port-forward` as managed background processes. They are the only
// long-running kubectl processes k8s-buddy starts; /api/exec still refuses port-forward.

const forwardStartTimeout = 20 * time.Second

var forwardKinds = map[string]string{"pods": "pod", "services": "svc", "deployments": "deployment", "statefulsets": "statefulset"}

type Forward struct {
	ID         string    `json:"id"`
	Cluster    string    `json:"cluster"`
	Namespace  string    `json:"namespace"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	RemotePort int       `json:"remotePort"`
	LocalPort  int       `json:"localPort"`
	URL        string    `json:"url"`
	State      string    `json:"state"` // starting, active, failed
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"startedAt"`

	cmd     *exec.Cmd
	stopped bool
	tail    []string
}

type forwardRequest struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Port      int    `json:"port"`
	LocalPort int    `json:"localPort,omitempty"`
}

type forwarder struct {
	mu   sync.Mutex
	list []*Forward
	env  func() []string
}

func (f *forwarder) snapshot() []Forward {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Forward, 0, len(f.list))
	for _, x := range f.list {
		c := *x
		c.cmd, c.tail = nil, nil
		out = append(out, c)
	}
	return out
}

// validName rejects anything that could be read as a flag or a resource path.
func validName(s string) bool {
	return s != "" && !strings.HasPrefix(s, "-") && !strings.ContainsAny(s, "/ \t\n\\")
}

func freePort(preferred int) (int, error) {
	if preferred > 0 {
		if ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(preferred)); err == nil {
			ln.Close()
			return preferred, nil
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func (f *forwarder) start(req forwardRequest) (Forward, error) {
	ref, ok := forwardKinds[req.Kind]
	if !ok {
		return Forward{}, fmt.Errorf("can't port-forward to %q", req.Kind)
	}
	if req.Cluster == "" || !validName(req.Namespace) || !validName(req.Name) {
		return Forward{}, errors.New("invalid target")
	}
	if req.Port < 1 || req.Port > 65535 {
		return Forward{}, errors.New("pick a port between 1 and 65535")
	}
	f.mu.Lock()
	for _, x := range f.list {
		if x.State != "failed" && x.Cluster == req.Cluster && x.Namespace == req.Namespace && x.Kind == req.Kind && x.Name == req.Name && x.RemotePort == req.Port {
			c := *x
			f.mu.Unlock()
			c.cmd, c.tail = nil, nil
			return c, nil
		}
	}
	f.mu.Unlock()

	local, err := freePort(req.LocalPort)
	if err != nil {
		return Forward{}, err
	}
	var id [6]byte
	_, _ = rand.Read(id[:])
	fw := &Forward{ID: hex.EncodeToString(id[:]), Cluster: req.Cluster, Namespace: req.Namespace, Kind: req.Kind, Name: req.Name,
		RemotePort: req.Port, LocalPort: local, URL: fmt.Sprintf("http://127.0.0.1:%d", local), State: "starting", StartedAt: time.Now()}
	cmd := exec.Command("kubectl", "--context", req.Cluster, "-n", req.Namespace, "port-forward", "--address", "127.0.0.1",
		ref+"/"+req.Name, fmt.Sprintf("%d:%d", local, req.Port))
	cmd.Env = f.env()
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return Forward{}, err
	}
	fw.cmd = cmd
	f.mu.Lock()
	f.list = append(f.list, fw)
	f.mu.Unlock()

	ready := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(pr)
		signalled := false
		for sc.Scan() {
			line := sc.Text()
			f.mu.Lock()
			fw.tail = append(fw.tail, line)
			if len(fw.tail) > 20 {
				fw.tail = fw.tail[len(fw.tail)-20:]
			}
			if !signalled && strings.Contains(line, "Forwarding from") {
				fw.State, signalled = "active", true
				close(ready)
			}
			// kubectl keeps running after the pod goes away but every connection fails; surface that.
			if strings.Contains(line, "lost connection to pod") || strings.Contains(line, "an error occurred forwarding") {
				fw.Error = strings.TrimPrefix(line, "E")
			}
			f.mu.Unlock()
		}
	}()
	go func() {
		err := cmd.Wait()
		pw.Close()
		f.mu.Lock()
		if fw.stopped {
			f.remove(fw.ID)
		} else {
			fw.State = "failed"
			msg := strings.TrimSpace(strings.Join(fw.tail, "\n"))
			if msg == "" && err != nil {
				msg = err.Error()
			}
			if msg == "" {
				msg = "kubectl port-forward exited"
			}
			fw.Error = lastLines(msg, 3)
		}
		f.mu.Unlock()
		close(exited)
	}()
	select {
	case <-ready:
	case <-exited:
	case <-time.After(forwardStartTimeout):
		f.stop(fw.ID)
		return Forward{}, errors.New("port-forward didn't start within 20s")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c := *fw
	c.cmd, c.tail = nil, nil
	if c.State == "failed" {
		f.remove(fw.ID)
		return Forward{}, errors.New(c.Error)
	}
	return c, nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// remove must be called with f.mu held.
func (f *forwarder) remove(id string) {
	f.list = slices.DeleteFunc(f.list, func(x *Forward) bool { return x.ID == id })
}

func (f *forwarder) stop(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.list {
		if x.ID != id {
			continue
		}
		if x.State == "failed" {
			f.remove(id)
			return true
		}
		x.stopped = true
		if x.cmd != nil && x.cmd.Process != nil {
			_ = x.cmd.Process.Kill()
		}
		return true
	}
	return false
}

func (f *forwarder) stopAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.list {
		x.stopped = true
		if x.cmd != nil && x.cmd.Process != nil {
			_ = x.cmd.Process.Kill()
		}
	}
}

// StopForwards kills every port-forward this app started; call it on quit.
func (s *Server) StopForwards() { s.forwards.stopAll() }

func (s *Server) kubectlEnv() []string {
	env := os.Environ()
	if kc := s.settings.Get().Kubeconfig; kc != "" {
		env = append(env, "KUBECONFIG="+kc)
	}
	return env
}

func (s *Server) handleForwardStart(w http.ResponseWriter, r *http.Request) {
	var req forwardRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.mgr.Connected(req.Cluster) {
		http.Error(w, "cluster "+strconv.Quote(req.Cluster)+" is not connected", http.StatusBadRequest)
		return
	}
	fw, err := s.forwards.start(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, fw)
}

// handleForwardOpen opens an active forward's URL in the default browser. The embedded web views
// don't open new windows, and only URLs of forwards we started can be opened this way.
func (s *Server) handleForwardOpen(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var url string
	for _, x := range s.forwards.snapshot() {
		if x.ID == id && x.State == "active" {
			url = x.URL
		}
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
}

func openURL(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Run()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Run()
	default:
		return exec.Command("xdg-open", url).Run()
	}
}
