package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestIsReleaseAndNewer(t *testing.T) {
	for v, want := range map[string]bool{
		"0.1.16": true, "v1.2.3": true, "10.20.30": true,
		"dev": false, "": false, "0.1.16-rc.1": false, "0.1.16-3-gabc123": false, "0.1.16-dirty": false, "1.2": false,
	} {
		if IsRelease(v) != want {
			t.Errorf("IsRelease(%q) = %v", v, !want)
		}
	}
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.1.17", "0.1.16", true}, {"0.2.0", "0.1.99", true}, {"0.1.10", "0.1.9", true},
		{"0.1.16", "0.1.16", false}, {"0.1.15", "0.1.16", false},
		{"0.1.17-rc.1", "0.1.16", false}, {"0.1.17", "dev", false},
	}
	for _, c := range cases {
		if Newer(c.a, c.b) != c.want {
			t.Errorf("Newer(%s, %s) = %v", c.a, c.b, !c.want)
		}
	}
}

const body = `{"tag_name":"v0.1.17","html_url":"https://github.com/dlebee/k8s-buddy/releases/tag/v0.1.17","draft":false,"prerelease":false,
"assets":[{"name":"k8s-buddy-0.1.17-macos-universal.dmg","browser_download_url":"https://github.com/dlebee/k8s-buddy/releases/download/v0.1.17/k8s-buddy-0.1.17-macos-universal.dmg"},
{"name":"k8s-buddy-0.1.17-windows-x64.msi","browser_download_url":"https://github.com/dlebee/k8s-buddy/releases/download/v0.1.17/k8s-buddy-0.1.17-windows-x64.msi"},
{"name":"k8s-buddy-0.1.17-windows-arm64.msi","browser_download_url":"https://github.com/dlebee/k8s-buddy/releases/download/v0.1.17/k8s-buddy-0.1.17-windows-arm64.msi"},
{"name":"k8s-buddy-0.1.17-linux-amd64.tar.gz","browser_download_url":"https://evil.example.com/k8s-buddy-0.1.17-linux-amd64.tar.gz"}]}`

func server(t *testing.T, payload string, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("If-None-Match") == `"v17"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v17"`)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func checker(url, current, goos, goarch string) *Checker {
	c := New(current, func() bool { return true }, nil)
	c.url, c.goos, c.goarch = url, goos, goarch
	return c
}

func TestCheckFindsTheFileForThisComputer(t *testing.T) {
	var hits atomic.Int32
	srv := server(t, body, &hits)
	for _, tc := range []struct{ goos, goarch, file string }{
		{"darwin", "arm64", "k8s-buddy-0.1.17-macos-universal.dmg"},
		{"windows", "amd64", "k8s-buddy-0.1.17-windows-x64.msi"},
		{"windows", "arm64", "k8s-buddy-0.1.17-windows-arm64.msi"},
		{"linux", "amd64", ""}, // that link points outside the repository's releases: not offered
	} {
		c := checker(srv.URL, "0.1.16", tc.goos, tc.goarch)
		c.check(context.Background())
		st := c.Status()
		if !st.Available || st.Latest != "0.1.17" || st.File != tc.file || st.Notes != "https://github.com/dlebee/k8s-buddy/releases/tag/v0.1.17" || st.Error != "" {
			t.Fatalf("%s/%s: %+v", tc.goos, tc.goarch, st)
		}
	}
	// Unchanged since the last check: GitHub answers 304 and the result stays.
	c := checker(srv.URL, "0.1.16", "darwin", "arm64")
	c.check(context.Background())
	c.check(context.Background())
	if st := c.Status(); !st.Available || st.Latest != "0.1.17" {
		t.Fatalf("after 304: %+v", st)
	}
	// Already on it, or newer.
	for _, cur := range []string{"0.1.17", "0.2.0"} {
		c := checker(srv.URL, cur, "darwin", "arm64")
		c.check(context.Background())
		if c.Status().Available {
			t.Fatalf("%s: should not offer 0.1.17", cur)
		}
	}
}

func TestPreReleasesAreNeverOffered(t *testing.T) {
	var hits atomic.Int32
	for _, payload := range []string{
		`{"tag_name":"v0.1.17-rc.1","html_url":"https://github.com/dlebee/k8s-buddy/releases/tag/v0.1.17-rc.1","prerelease":true}`,
		`{"tag_name":"v0.1.17","html_url":"https://github.com/dlebee/k8s-buddy/releases/tag/v0.1.17","prerelease":true}`,
		`{"tag_name":"v0.1.17-rc.1","html_url":"https://github.com/dlebee/k8s-buddy/releases/tag/v0.1.17-rc.1"}`,
	} {
		c := checker(server(t, payload, &hits).URL, "0.1.16", "darwin", "arm64")
		c.check(context.Background())
		if st := c.Status(); st.Available || st.Latest != "" {
			t.Fatalf("%s: %+v", payload, st)
		}
	}
}

func TestLocalBuildsNeverCheck(t *testing.T) {
	var hits atomic.Int32
	srv := server(t, body, &hits)
	for _, v := range []string{"dev", "0.1.16-3-gabc123-dirty", "0.1.17-rc.1"} {
		c := checker(srv.URL, v, "darwin", "arm64")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { c.Run(ctx); close(done) }()
		c.CheckNow()
		<-done // Run returns at once for builds that aren't releases
		cancel()
	}
	if hits.Load() != 0 {
		t.Fatalf("GitHub was asked %d times", hits.Load())
	}
}

func TestOffWhenDisabled(t *testing.T) {
	var hits atomic.Int32
	c := checker(server(t, body, &hits).URL, "0.1.16", "darwin", "arm64")
	c.check(context.Background())
	c.enabled = func() bool { return false }
	if st := c.Status(); st.Available || st.Enabled {
		t.Fatalf("turned off: %+v", st)
	}
}
