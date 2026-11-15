package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

const (
	execTimeout   = 60 * time.Second
	execMaxOutput = 512 << 10
)

var allowedBins = map[string]bool{"kubectl": true, "helm": true, "cmctl": true}

// Subcommands that change cluster state and therefore need an explicit confirmation.
var mutating = map[string][]string{
	"kubectl": {"delete", "apply", "create", "replace", "patch", "scale", "autoscale", "drain", "cordon", "uncordon",
		"taint", "label", "annotate", "set", "rollout restart", "rollout undo", "rollout pause", "rollout resume", "expose", "run", "cp"},
	"helm":  {"install", "upgrade", "rollback", "uninstall", "delete", "test"},
	"cmctl": {"renew", "approve", "deny", "create", "x"},
}

// Commands that need a terminal or never finish.
var interactive = map[string][]string{
	"kubectl": {"edit", "exec", "attach", "port-forward", "proxy", "debug", "logs -f", "logs --follow", "get -w", "get --watch", "rollout status -w"},
	"helm":    {},
	"cmctl":   {},
}

// splitArgs tokenizes a command line like a shell would for words and quotes, but rejects any
// shell metacharacter outside quotes: commands are executed directly, never through a shell.
func splitArgs(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inWord := false
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
		case strings.ContainsRune("|;&<>`$()\n\\*?{}", r):
			return nil, errors.New("shell syntax (pipes, redirects, variables, globs) isn't supported — run one command at a time")
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}
	if inWord {
		args = append(args, cur.String())
	}
	return args, nil
}

// verbs drops flags (and the values of flags we know take one) so "kubectl --context x -n y delete pod z"
// yields ["delete", "pod", "z"].
func verbs(args []string) []string {
	takesValue := map[string]bool{"--context": true, "-n": true, "--namespace": true, "--kubeconfig": true, "--kube-context": true,
		"-o": true, "--output": true, "-c": true, "--container": true, "-l": true, "--selector": true, "--tail": true, "--since": true}
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			if takesValue[a] {
				i++
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

func hasFlag(args []string, flags ...string) bool {
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		if slices.Contains(flags, name) {
			return true
		}
	}
	return false
}

type commandCheck struct {
	argv     []string
	mutating string
}

func checkCommand(line string) (commandCheck, error) {
	argv, err := splitArgs(strings.TrimSpace(line))
	if err != nil {
		return commandCheck{}, err
	}
	if len(argv) == 0 {
		return commandCheck{}, errors.New("empty command")
	}
	bin := argv[0]
	if !allowedBins[bin] {
		return commandCheck{}, errors.New("only kubectl, helm and cmctl can be run here")
	}
	v := verbs(argv[1:])
	joined := strings.Join(v, " ")
	matches := func(prefixes []string) string {
		for _, p := range prefixes {
			if joined == p || strings.HasPrefix(joined, p+" ") {
				return p
			}
		}
		return ""
	}
	for _, p := range interactive[bin] {
		pv, flag, _ := strings.Cut(p, " -")
		if flag != "" {
			if matches([]string{pv}) != "" && hasFlag(argv, "-"+flag) {
				return commandCheck{}, errors.New("`" + p + "` streams forever — use a terminal for that")
			}
			continue
		}
		if matches([]string{p}) != "" {
			return commandCheck{}, errors.New("`" + bin + " " + p + "` is interactive — use a terminal for that")
		}
	}
	if hasFlag(argv, "-i", "-it", "-ti", "--stdin", "--tty", "-t") && bin == "kubectl" && slices.Contains(v, "run") {
		return commandCheck{}, errors.New("interactive commands need a terminal")
	}
	return commandCheck{argv: argv, mutating: matches(mutating[bin])}, nil
}

type execRequest struct {
	Command string `json:"command"`
	Confirm bool   `json:"confirm"`
}

type execResponse struct {
	Command       string `json:"command"`
	ExitCode      int    `json:"exitCode"`
	Output        string `json:"output"`
	Truncated     bool   `json:"truncated,omitempty"`
	DurationMs    int64  `json:"durationMs"`
	NeedsConfirm  bool   `json:"needsConfirm,omitempty"`
	ConfirmReason string `json:"confirmReason,omitempty"`
	Error         string `json:"error,omitempty"`
}

type limitedBuffer struct {
	bytes.Buffer
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := execMaxOutput - b.Len(); room < len(p) {
		b.truncated = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	var req execRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp := execResponse{Command: req.Command}
	chk, err := checkCommand(req.Command)
	if err != nil {
		resp.Error, resp.ExitCode = err.Error(), -1
		writeJSON(w, resp)
		return
	}
	if chk.mutating != "" && !req.Confirm {
		resp.NeedsConfirm = true
		resp.ConfirmReason = "`" + chk.argv[0] + " " + chk.mutating + "` changes your cluster."
		writeJSON(w, resp)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, chk.argv[0], chk.argv[1:]...)
	// Commands must see the same kubeconfig the monitors use.
	if kc := s.settings.Get().Kubeconfig; kc != "" {
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kc)
	}
	var out limitedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	start := time.Now()
	err = cmd.Run()
	resp.DurationMs = time.Since(start).Milliseconds()
	resp.Output, resp.Truncated = out.String(), out.truncated
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		resp.ExitCode = exitErr.ExitCode()
	case ctx.Err() == context.DeadlineExceeded:
		resp.ExitCode, resp.Error = -1, "timed out after 60s"
	case err != nil:
		resp.ExitCode, resp.Error = -1, err.Error()
	}
	writeJSON(w, resp)
}
