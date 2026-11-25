package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Releases set the version with -ldflags "-X main.version=…". If the variable is renamed or
// moved, -X silently does nothing, every release reports "dev" and the update check never runs.
func TestVersionIsSetAtBuildTime(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the app")
	}
	bin := filepath.Join(t.TempDir(), "k8s-buddy")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-ldflags", "-X main.version=9.8.7", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, "-version").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "k8s-buddy 9.8.7" {
		t.Fatalf("-version printed %q", got)
	}
}
