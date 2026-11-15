package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
)

// Shells open in the user's own terminal app rather than inside k8s-buddy: /api/exec never
// runs interactive commands, and a real terminal gets job control, resizing and copy/paste right.

type shellRequest struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Container string `json:"container"`
}

type shellResponse struct {
	Command string `json:"command"`
	Opened  bool   `json:"opened"`
	Error   string `json:"error,omitempty"`
}

// shQuote single-quotes s for a POSIX shell.
func shQuote(s string) string {
	if s != "" && !strings.ContainsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.:/@=", r))
	}) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Prefer bash, fall back to sh, without running sh after a bash session exits non-zero.
const shellProbe = "command -v bash >/dev/null 2>&1 && exec bash || exec sh"

func shellCommand(req shellRequest) string {
	return "kubectl --context " + shQuote(req.Cluster) + " -n " + shQuote(req.Namespace) + " exec -it " + shQuote(req.Pod) +
		" -c " + shQuote(req.Container) + " -- sh -c " + shQuote(shellProbe)
}

// shellScript is a .command file: Terminal.app runs it without needing Apple Events permission.
func shellScript(req shellRequest, kubeconfig string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\nrm -f \"$0\"\n")
	fmt.Fprintf(&b, "printf '\\033]0;%%s\\007' %s\n", shQuote(req.Container+" @ "+req.Pod))
	if kubeconfig != "" {
		b.WriteString("export KUBECONFIG=" + shQuote(kubeconfig) + "\n")
	}
	fmt.Fprintf(&b, "echo %s\n", shQuote("→ "+req.Cluster+" · "+req.Namespace+"/"+req.Pod+" · "+req.Container))
	b.WriteString(shellCommand(req) + "\n")
	return b.String()
}

func openTerminal(script string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("opening a terminal is only supported on macOS — copy the command instead")
	}
	f, err := os.CreateTemp("", "k8s-buddy-shell-*.command")
	if err != nil {
		return err
	}
	if _, err := f.WriteString(script); err != nil {
		f.Close()
		return err
	}
	f.Close()
	if err := os.Chmod(f.Name(), 0o700); err != nil {
		return err
	}
	if out, err := exec.Command("open", "-a", "Terminal", f.Name()).CombinedOutput(); err != nil {
		os.Remove(f.Name())
		return fmt.Errorf("open Terminal: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *Server) handleShell(w http.ResponseWriter, r *http.Request) {
	var req shellRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Only shells into containers that actually exist on a monitored cluster.
	containers, err := s.mgr.PodContainers(req.Cluster, req.Namespace, req.Pod)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if req.Container == "" && len(containers) > 0 {
		req.Container = containers[0]
	}
	if !slices.Contains(containers, req.Container) {
		http.Error(w, "no container "+req.Container+" in pod "+req.Pod, http.StatusBadRequest)
		return
	}
	resp := shellResponse{Command: shellCommand(req)}
	if err := openTerminal(shellScript(req, s.settings.Get().Kubeconfig)); err != nil {
		resp.Error = err.Error()
	} else {
		resp.Opened = true
	}
	writeJSON(w, resp)
}
