package monitor

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dlebee/k8s-buddy/internal/alert"
)

// StatusSignIn means the cluster is fine but its credentials need a fresh `az login`.
const StatusSignIn Status = "signin"

// AzureCluster is the pseudo-cluster the account-wide sign-in alert is filed under.
const AzureCluster = "Azure"

const (
	azureCheckTTL     = time.Minute
	azureRecheckEvery = time.Minute
	azureLoginTimeout = 5 * time.Minute
)

// azCommand is the Azure CLI binary; tests swap it for a fake.
var azCommand = "az"

type azureState struct {
	checkMu   sync.Mutex // serialises `az account get-access-token`
	mu        sync.Mutex
	checkedAt time.Time
	signedIn  bool
	reason    string
	waiting   map[string]bool
	watching  bool
	login     AzureLogin
}

type AzureLogin struct {
	Running   bool      `json:"running"`
	StartedAt time.Time `json:"startedAt,omitzero"`
	Error     string    `json:"error,omitempty"`
}

type AzureStatus struct {
	Needed   bool       `json:"needed"`
	Reason   string     `json:"reason,omitempty"`
	Clusters []string   `json:"clusters"`
	Login    AzureLogin `json:"login"`
}

// isCredentialError matches failures of the kubeconfig exec plugin rather than the API server.
func isCredentialError(err error) bool {
	msg := err.Error()
	for _, s := range []string{"getting credentials", "executable kubelogin", "AADSTS", "az login", "AzureCLICredential", "exec plugin"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// azureSignedIn asks the Azure CLI whether its session can still mint tokens. The answer is
// cached briefly and shared, so 22 clusters failing at once cost a single `az` call.
func (m *Manager) azureSignedIn(force bool) (bool, string) {
	m.az.checkMu.Lock()
	defer m.az.checkMu.Unlock()
	m.az.mu.Lock()
	if !force && !m.az.checkedAt.IsZero() && time.Since(m.az.checkedAt) < azureCheckTTL {
		ok, reason := m.az.signedIn, m.az.reason
		m.az.mu.Unlock()
		return ok, reason
	}
	m.az.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, azCommand, "account", "get-access-token", "--output", "none", "--only-show-errors")
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	ok, reason := err == nil, ""
	if !ok {
		reason = firstLine(out.String())
		if reason == "" {
			reason = err.Error()
		}
	}
	m.az.mu.Lock()
	m.az.signedIn, m.az.reason, m.az.checkedAt = ok, truncate(reason, 300), time.Now()
	m.az.mu.Unlock()
	return ok, reason
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "ERROR:"))
		if l != "" {
			return l
		}
	}
	return ""
}

// onAzureCredentialFailure is called when an azurecli context can't get credentials. It returns
// true when an expired Azure session explains the failure; the caller then skips its own
// connectivity alert.
func (m *Manager) onAzureCredentialFailure(c *cluster) bool {
	if ok, _ := m.azureSignedIn(false); ok {
		return false
	}
	m.az.mu.Lock()
	if m.az.waiting == nil {
		m.az.waiting = map[string]bool{}
	}
	m.az.waiting[c.name] = true
	startWatch := !m.az.watching
	m.az.watching = true
	m.az.mu.Unlock()

	c.mu.Lock()
	c.status = StatusSignIn
	c.mu.Unlock()
	// The account-wide alert speaks for every waiting cluster; drop any per-cluster connectivity
	// alert so one expired session doesn't read as N broken clusters.
	c.store.ResolveMissing(c.name, "connectivity", nil)
	m.raiseAzureAlert()
	if startWatch {
		go m.watchAzure()
	}
	return true
}

func (m *Manager) raiseAzureAlert() {
	m.az.mu.Lock()
	n, reason := len(m.az.waiting), m.az.reason
	m.az.mu.Unlock()
	m.store.Observe(alert.Alert{
		Key: "azure-signin", Source: "azure", Cluster: AzureCluster, Severity: alert.Warning, Category: "auth",
		Title:  fmt.Sprintf("Azure sign-in expired · %d cluster%s waiting", n, map[bool]string{true: "", false: "s"}[n == 1]),
		Detail: joinNonEmpty(" · ", reason, "Sign in again to reconnect them all."),
		Hint:   "az login",
		Count:  n,
	})
}

// watchAzure rechecks the session until it works again (from our button or a terminal `az login`).
func (m *Manager) watchAzure() {
	t := time.NewTicker(azureRecheckEvery)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
		}
		if ok, _ := m.azureSignedIn(true); ok {
			m.azureRestored()
			return
		}
	}
}

func (m *Manager) azureRestored() {
	m.az.mu.Lock()
	m.az.waiting = nil
	m.az.watching = false
	m.az.mu.Unlock()
	m.store.ResolveMissing(AzureCluster, "azure", nil)
	m.RetryAll()
}

// AzureStatus reports whether an Azure sign-in is needed and which clusters wait on it.
func (m *Manager) AzureStatus() AzureStatus {
	m.az.mu.Lock()
	defer m.az.mu.Unlock()
	st := AzureStatus{Login: m.az.login, Clusters: []string{}}
	for name := range m.az.waiting {
		st.Clusters = append(st.Clusters, name)
	}
	slices.Sort(st.Clusters)
	st.Needed = len(st.Clusters) > 0
	if st.Needed {
		st.Reason = m.az.reason
	}
	return st
}

// StartAzureLogin runs `az login`, which opens the browser for the usual Microsoft sign-in, and
// reconnects every cluster once it succeeds.
func (m *Manager) StartAzureLogin() AzureStatus {
	m.az.mu.Lock()
	if m.az.login.Running {
		m.az.mu.Unlock()
		return m.AzureStatus()
	}
	m.az.login = AzureLogin{Running: true, StartedAt: time.Now()}
	m.az.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(m.ctx, azureLoginTimeout)
		defer cancel()
		var out bytes.Buffer
		cmd := exec.CommandContext(ctx, azCommand, "login", "--output", "none", "--only-show-errors")
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		errMsg := ""
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			errMsg = "sign-in timed out after 5 minutes"
		case err != nil:
			errMsg = firstNonEmpty(firstLine(out.String()), err.Error())
		}
		if errMsg == "" {
			if ok, reason := m.azureSignedIn(true); !ok {
				errMsg = "signed in, but tokens still fail: " + reason
			}
		}
		m.az.mu.Lock()
		m.az.login.Running, m.az.login.Error = false, truncate(errMsg, 300)
		m.az.mu.Unlock()
		if errMsg == "" {
			m.azureRestored()
		}
		m.store.Touch()
	}()
	return m.AzureStatus()
}

// RetryAll makes every cluster retry its connection now instead of waiting out its backoff.
func (m *Manager) RetryAll() {
	for _, c := range m.connected() {
		select {
		case c.retry <- struct{}{}:
		default:
		}
	}
}
