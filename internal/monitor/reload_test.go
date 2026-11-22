package monitor

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/dlebee/k8s-buddy/internal/alert"
	"github.com/dlebee/k8s-buddy/internal/config"
)

// kubeconfig with the named contexts, all pointing at an address nothing listens on.
func writeKubeconfig(t *testing.T, path string, names ...string) {
	t.Helper()
	s := "apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster: {server: 'https://127.0.0.1:1'}\nusers:\n- name: u\n  user: {token: x}\ncontexts:\n"
	for _, n := range names {
		s += "- name: " + n + "\n  context: {cluster: c, user: u}\n"
	}
	// Swap in a complete file, so a background reload never reads it half-written.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func TestReloadNowReportsNewAndRemovedContexts(t *testing.T) {
	dir := t.TempDir()
	kc := filepath.Join(dir, "kubeconfig")
	writeKubeconfig(t, kc, "a", "b")
	st, err := config.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(s *config.Settings) { s.Kubeconfig = kc }); err != nil {
		t.Fatal(err)
	}
	m := NewManager(st, alert.NewStore(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	wait, done := context.WithTimeout(ctx, 10*time.Second)
	defer done()
	if _, err := m.ReloadNow(wait); err != nil {
		t.Fatal(err)
	}
	writeKubeconfig(t, kc, "b", "c")
	r, err := m.ReloadNow(wait)
	if err != nil {
		t.Fatal(err)
	}
	if r.Contexts != 2 || !slices.Equal(r.Added, []string{"c"}) || !slices.Equal(r.Removed, []string{"a"}) || r.Error != "" {
		t.Fatalf("reload: %+v", r)
	}
}
