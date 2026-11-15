package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// A small file in the data directory lets a second launch (e.g. the Windows Start-menu
// shortcut) find the running instance instead of starting another one.

type instanceInfo struct {
	PID int    `json:"pid"`
	URL string `json:"url"`
}

func instancePath(dataDir string) string { return filepath.Join(dataDir, "instance.json") }

// runningInstance returns the overview URL of a k8s-buddy already serving this data directory.
func runningInstance(dataDir string) (string, bool) {
	b, err := os.ReadFile(instancePath(dataDir))
	if err != nil {
		return "", false
	}
	var info instanceInfo
	if json.Unmarshal(b, &info) != nil || info.URL == "" {
		return "", false
	}
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(info.URL)
	if err != nil {
		return "", false
	}
	resp.Body.Close()
	return info.URL, resp.StatusCode == http.StatusOK
}

// claimInstance records this process as the running instance; the returned func releases it.
func claimInstance(dataDir, url string) func() {
	path := instancePath(dataDir)
	b, _ := json.Marshal(instanceInfo{PID: os.Getpid(), URL: url})
	// Write-then-rename so an existing file's looser mode can't be inherited.
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) != nil || os.Chmod(tmp, 0o600) != nil || os.Rename(tmp, path) != nil {
		os.Remove(tmp)
		return func() {}
	}
	return func() {
		// Only remove the file if it still describes us.
		if cur, err := os.ReadFile(path); err == nil && string(cur) == string(b) {
			os.Remove(path)
		}
	}
}
