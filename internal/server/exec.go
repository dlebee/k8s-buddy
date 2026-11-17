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

type commandRule int

const (
	readOnly commandRule = iota + 1
	needsConfirm
)

// commands is an allowlist: anything not listed (proxy, cp, exec, edit, config, plugins, helm plugin,
// repo, template, ...) is refused. Keys are the command words, e.g. "rollout restart".
var commands = map[string]map[string]commandRule{
	"kubectl": {
		"get": readOnly, "describe": readOnly, "logs": readOnly, "top": readOnly, "events": readOnly,
		"explain": readOnly, "api-resources": readOnly, "api-versions": readOnly, "version": readOnly,
		"cluster-info": readOnly, "rollout history": readOnly, "rollout status": readOnly,
		"auth can-i": readOnly, "auth whoami": readOnly,
		"delete": needsConfirm, "apply": needsConfirm, "create": needsConfirm, "replace": needsConfirm,
		"patch": needsConfirm, "scale": needsConfirm, "label": needsConfirm, "annotate": needsConfirm,
		"set": needsConfirm, "cordon": needsConfirm, "uncordon": needsConfirm, "drain": needsConfirm,
		"taint": needsConfirm, "rollout restart": needsConfirm, "rollout undo": needsConfirm,
		"rollout pause": needsConfirm, "rollout resume": needsConfirm,
		"certificate approve": needsConfirm, "certificate deny": needsConfirm,
	},
	"helm": {
		"status": readOnly, "history": readOnly, "list": readOnly, "ls": readOnly, "version": readOnly,
		"get manifest": needsConfirm, "get notes": readOnly, // rendered Secrets are in the manifest "get hooks": readOnly, "get metadata": readOnly,
		"get values": needsConfirm, "get all": needsConfirm, // values often hold credentials
		"rollback": needsConfirm, "uninstall": needsConfirm, "un": needsConfirm, "delete": needsConfirm, "del": needsConfirm,
	},
	"cmctl": {
		"status certificate": readOnly, "check api": readOnly, "version": readOnly,
		"renew": needsConfirm, "approve": needsConfirm, "deny": needsConfirm,
	},
}

// Options allowed before the command word, and whether they take a value. Anything else must come
// after the command, so an unknown flag's value can never be mistaken for the command.
var leadingFlags = map[string]map[string]bool{
	"kubectl": {"--context": true, "-n": true, "--namespace": true, "-A": false, "--all-namespaces": false},
	"helm":    {"--kube-context": true, "-n": true, "--namespace": true, "-A": false, "--all-namespaces": false},
	"cmctl":   {"--context": true, "-n": true, "--namespace": true},
}

// Flags that change which cluster or identity is used, write local files or run local programs.
// They are refused anywhere on the line.
var forbiddenFlags = map[string]bool{
	"--server": true, "--kube-apiserver": true, "--insecure-skip-tls-verify": true, "--kube-insecure-skip-tls-verify": true,
	"--kubeconfig": true, "--token": true, "--kube-token": true, "--as": true, "--as-group": true, "--as-uid": true,
	"--kube-as-user": true, "--kube-as-group": true, "--user": true, "--cluster": true, "--username": true, "--password": true,
	"--certificate-authority": true, "--client-certificate": true, "--client-key": true, "--kube-ca-file": true,
	"--tls-server-name": true, "--kube-tls-server-name": true, "--profile": true, "--profile-output": true,
	"--cache-dir": true, "--post-renderer": true, "--post-renderer-args": true, "--repository-config": true,
	"--repository-cache": true, "--registry-config": true, "--plugins": true, "--address": true, "--accept-hosts": true,
	"--log-file": true, "--log-dir": true, // older kubectl/klog: write local files
}

// Single-letter kubectl/cmctl flags that take no value, so they can be grouped before another
// shorthand ("-As" is "-A -s").
const boolShorthands = "AwRith"

// flagName is the flag an argument sets, the way kubectl's flag parser reads it: the part before
// "=", with "_" read as "-" (kubectl accepts --insecure_skip_tls_verify for --insecure-skip-tls-verify).
func flagName(a string) string {
	name, _, _ := strings.Cut(a, "=")
	if strings.HasPrefix(name, "--") {
		name = strings.ReplaceAll(name, "_", "-")
	}
	return name
}

// setsServerShorthand reports whether a single-dash argument sets kubectl's -s (--server): "-s",
// "-shttps://…" (value attached) or grouped behind value-less shorthands ("-As").
func setsServerShorthand(a string) bool {
	if !strings.HasPrefix(a, "-") || strings.HasPrefix(a, "--") {
		return false
	}
	for _, r := range a[1:] {
		if r == 's' {
			return true
		}
		if !strings.ContainsRune(boolShorthands, r) {
			return false // a flag that takes a value: the rest of the argument is that value
		}
	}
	return false
}

// Flags that make a read-only command stream forever.
var streamingFlags = map[string]bool{"-f": true, "--follow": true, "-w": true, "--watch": true, "--watch-only": true}

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

type commandCheck struct {
	argv    []string
	command string // e.g. "rollout restart"
	confirm string // non-empty: why the user must confirm before it runs
}

func checkCommand(line string) (commandCheck, error) {
	line = strings.TrimSpace(line)
	for _, r := range line {
		if r < 0x20 || r > 0x7e {
			return commandCheck{}, errors.New("only plain ASCII text is allowed in commands")
		}
	}
	argv, err := splitArgs(line)
	if err != nil {
		return commandCheck{}, err
	}
	if len(argv) == 0 {
		return commandCheck{}, errors.New("empty command")
	}
	bin, args := argv[0], argv[1:]
	if !allowedBins[bin] {
		return commandCheck{}, errors.New("only kubectl, helm and cmctl can be run here")
	}
	for _, a := range args {
		name := flagName(a)
		if forbiddenFlags[name] || (bin != "helm" && setsServerShorthand(a)) {
			if !forbiddenFlags[name] {
				name = "-s"
			}
			return commandCheck{}, errors.New("`" + name + "` isn't allowed here: it changes the cluster, identity or local files a command uses")
		}
	}

	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		name, _, hasValue := strings.Cut(args[i], "=")
		takesValue, ok := leadingFlags[bin][name]
		if !ok {
			return commandCheck{}, errors.New("put `" + name + "` after the command (e.g. `" + bin + " get … " + name + "`)")
		}
		i++
		if takesValue && !hasValue {
			if i >= len(args) {
				return commandCheck{}, errors.New("`" + name + "` needs a value")
			}
			i++
		}
	}
	if i >= len(args) {
		return commandCheck{}, errors.New("missing command")
	}
	cmd, rule := args[i], commands[bin][args[i]]
	if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
		if r, ok := commands[bin][args[i]+" "+args[i+1]]; ok {
			cmd, rule = args[i]+" "+args[i+1], r
		}
	}
	if rule == 0 {
		return commandCheck{}, errors.New("`" + bin + " " + args[i] + "` isn't available in the console — use a terminal for it")
	}
	rest := args[i+1:]
	if rule == readOnly {
		for _, a := range rest {
			name, _, _ := strings.Cut(a, "=")
			if streamingFlags[name] {
				return commandCheck{}, errors.New("`" + name + "` streams forever — use a terminal for that")
			}
		}
	}
	chk := commandCheck{argv: argv, command: cmd}
	switch {
	case rule == needsConfirm:
		chk.confirm = "`" + bin + " " + cmd + "` changes your cluster or shows sensitive values."
	case bin == "kubectl" && cmd == "get" && printsRaw(rest):
		chk.confirm = "`--raw` prints API responses as they are, secret values included."
	case bin == "kubectl" && cmd == "get" && printsSecrets(rest):
		chk.confirm = "This prints secret values."
	}
	return chk, nil
}

// printsSecrets reports whether `kubectl get` args ask for Secret contents in a structured output.
// The kind may be qualified ("secrets.v1.", "secret.v1") and --template alone implies go-template.
func printsSecrets(args []string) bool {
	secret, output := false, false
	for _, a := range args {
		kind, _, _ := strings.Cut(strings.ToLower(a), "/")
		for _, k := range strings.Split(kind, ",") {
			if k, _, _ = strings.Cut(k, "."); k == "secret" || k == "secrets" {
				secret = true
			}
		}
		name := flagName(a)
		if name == "-o" || name == "--output" || name == "--template" || strings.HasPrefix(a, "-o") {
			output = true
		}
	}
	return secret && output
}

// printsRaw reports `kubectl get --raw <path>`, which can read any API path, Secrets included.
func printsRaw(args []string) bool {
	return slices.ContainsFunc(args, func(a string) bool { return flagName(a) == "--raw" })
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
	if chk.confirm != "" && !req.Confirm {
		resp.NeedsConfirm = true
		resp.ConfirmReason = chk.confirm
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
