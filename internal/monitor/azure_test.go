package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dlebee/k8s-buddy/internal/alert"
)

// fakeAz installs a stand-in Azure CLI whose session is expired until `az login` runs.
func fakeAz(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake")
	}
	dir := t.TempDir()
	flag := filepath.Join(dir, "signed-in")
	script := `#!/bin/sh
case "$1" in
  account) [ -f "` + flag + `" ] && exit 0
           echo "ERROR: AADSTS700082: The refresh token has expired due to inactivity." >&2; exit 1 ;;
  login)   touch "` + flag + `"; exit 0 ;;
esac
exit 2
`
	path := filepath.Join(dir, "az")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := azCommand
	azCommand = path
	t.Cleanup(func() { azCommand = old })
}

var kubeloginErr = errors.New(`Get "https://aks.example/version?timeout=20s": getting credentials: exec: executable kubelogin failed with exit code 1`)

func TestAzureSignInFlow(t *testing.T) {
	fakeAz(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := alert.NewStore()
	m := &Manager{store: store, ctx: ctx, running: map[string]*running{}}

	var clusters []*cluster
	for _, name := range []string{"aks-a", "aks-b", "aks-c"} {
		c := newCluster(name, "", nil, store)
		c.azureCLI, c.onAzureFailure = true, m.onAzureCredentialFailure
		m.running[name] = &running{c: c, cancel: func() {}}
		clusters = append(clusters, c)
		c.onFailure(kubeloginErr)
	}

	st := m.AzureStatus()
	if !st.Needed || len(st.Clusters) != 3 || !strings.Contains(st.Reason, "AADSTS700082") {
		t.Fatalf("status after failures: %+v", st)
	}
	signin := store.List(alert.Query{Scope: "active"})
	if len(signin) != 1 || signin[0].Key != "azure-signin" || signin[0].Severity != alert.Warning || !strings.Contains(signin[0].Title, "3 clusters waiting") {
		t.Fatalf("want one sign-in alert for 3 clusters, got %+v", signin)
	}
	for _, c := range clusters {
		if c.status != StatusSignIn {
			t.Errorf("%s status = %s", c.name, c.status)
		}
	}

	m.StartAzureLogin()
	deadline := time.Now().Add(10 * time.Second)
	for m.AzureStatus().Login.Running && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if st := m.AzureStatus(); st.Needed || st.Login.Error != "" {
		t.Fatalf("after login: %+v", st)
	}
	for _, a := range store.List(alert.Query{Scope: "active"}) {
		if a.Key == "azure-signin" {
			t.Fatal("sign-in alert should be resolved")
		}
	}
	for _, c := range clusters {
		select {
		case <-c.retry:
		default:
			t.Errorf("%s was not asked to retry", c.name)
		}
	}
}

func TestCredentialErrorWithValidAzureSessionIsNotSignIn(t *testing.T) {
	fakeAz(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := alert.NewStore()
	m := &Manager{store: store, ctx: ctx, running: map[string]*running{}}
	// Pretend the session is fine (e.g. the real problem is RBAC or a missing kubelogin).
	m.az.signedIn, m.az.checkedAt = true, time.Now()

	c := newCluster("aks", "", nil, store)
	c.azureCLI, c.onAzureFailure = true, m.onAzureCredentialFailure
	c.onFailure(kubeloginErr)
	if m.AzureStatus().Needed {
		t.Fatal("sign-in must not be reported while az works")
	}
	l := store.List(alert.Query{})
	if len(l) != 1 || l[0].Category != "connectivity" || l[0].Severity != alert.Notice {
		t.Fatalf("want the regular connectivity alert, got %+v", l)
	}
}

func TestIsCredentialError(t *testing.T) {
	if !isCredentialError(kubeloginErr) {
		t.Error("kubelogin exec failure should count")
	}
	if isCredentialError(errors.New(`Get "https://aks/version": dial tcp: lookup aks: no such host`)) {
		t.Error("DNS failure is not a credential error")
	}
}
