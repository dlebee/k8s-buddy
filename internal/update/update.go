// Package update tells the user when a newer k8s-buddy release is on GitHub. It never downloads
// or installs anything: it finds the release and the file for this computer, and the user
// downloads and installs it themselves.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	repo       = "dlebee/k8s-buddy"
	latestURL  = "https://api.github.com/repos/" + repo + "/releases/latest"
	releaseURL = "https://github.com/" + repo + "/releases/"
	checkEvery = 12 * time.Hour
	firstCheck = time.Minute // after start, so it doesn't compete with connecting to clusters
)

// Status is what the UI shows.
type Status struct {
	Current   string    `json:"current"`
	Checking  bool      `json:"checking"`
	Enabled   bool      `json:"enabled"` // checks are on in Settings
	Release   bool      `json:"release"` // this build is a release (dev and local builds never check)
	Latest    string    `json:"latest,omitempty"`
	Available bool      `json:"available"`          // Latest is newer than Current
	Notes     string    `json:"notes,omitempty"`    // the release page
	Download  string    `json:"download,omitempty"` // the file for this OS and CPU
	File      string    `json:"file,omitempty"`
	CheckedAt time.Time `json:"checkedAt,omitzero"`
	Error     string    `json:"error,omitempty"`
}

// Checker asks GitHub for the latest release now and then.
type Checker struct {
	current string
	enabled func() bool
	changed func() // called when Status changes
	client  *http.Client
	url     string // overridden in tests
	goos    string
	goarch  string

	mu   sync.Mutex
	st   Status
	etag string
	kick chan struct{}
}

func New(current string, enabled func() bool, changed func()) *Checker {
	return &Checker{
		current: current, enabled: enabled, changed: changed,
		client: &http.Client{Timeout: 20 * time.Second},
		url:    latestURL, goos: runtime.GOOS, goarch: runtime.GOARCH,
		st:   Status{Current: current, Release: IsRelease(current)},
		kick: make(chan struct{}, 1),
	}
}

var releaseRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)

// IsRelease reports whether v is a full release version like 1.2.3. Development builds ("dev"),
// local builds ("1.2.3-4-gabc123-dirty") and pre-releases ("1.2.3-rc.1") are not.
func IsRelease(v string) bool { return releaseRE.MatchString(v) }

func parse(v string) ([3]int, bool) {
	m := releaseRE.FindStringSubmatch(v)
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := range out {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// Newer reports whether release a is newer than b.
func Newer(a, b string) bool {
	x, ok1 := parse(a)
	y, ok2 := parse(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

func (c *Checker) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.st
	st.Enabled = c.enabled()
	if !st.Enabled {
		st.Available = false
	}
	return st
}

// CheckNow asks for a check as soon as possible (the Settings button).
func (c *Checker) CheckNow() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// Run checks shortly after start and then every 12 hours, while checks are on. Builds that
// aren't releases never contact GitHub.
func (c *Checker) Run(ctx context.Context) {
	if !IsRelease(c.current) {
		return
	}
	t := time.NewTimer(firstCheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if c.enabled() {
				c.check(ctx)
			}
			t.Reset(checkEvery)
		case <-c.kick:
			c.check(ctx)
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			t.Reset(checkEvery)
		}
	}
}

func (c *Checker) set(f func(*Status)) {
	c.mu.Lock()
	f(&c.st)
	c.mu.Unlock()
	if c.changed != nil {
		c.changed()
	}
}

func (c *Checker) check(ctx context.Context) {
	c.set(func(s *Status) { s.Checking = true })
	rel, err := c.fetch(ctx)
	c.set(func(s *Status) {
		s.Checking = false
		s.CheckedAt = time.Now()
		if err != nil {
			s.Error = err.Error()
			return
		}
		s.Error = ""
		if rel == nil { // not modified since the last check
			return
		}
		s.Latest = strings.TrimPrefix(rel.Tag, "v")
		s.Available = Newer(s.Latest, c.current)
		s.Notes = rel.Page
		s.Download, s.File = rel.asset(c.goos, c.goarch)
	})
}

type release struct {
	Tag        string `json:"tag_name"`
	Page       string `json:"html_url"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// fetch returns the latest full release, or nil when it hasn't changed since the last check.
// GitHub's "latest" is already the newest release that is neither a draft nor a pre-release;
// it's checked again here anyway.
func (c *Checker) fetch(ctx context.Context) (*release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "k8s-buddy/"+c.current)
	c.mu.Lock()
	if c.etag != "" && c.st.Latest != "" {
		req.Header.Set("If-None-Match", c.etag)
	}
	c.mu.Unlock()
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, errors.New("couldn't reach GitHub")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified:
		return nil, nil
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var rel release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, errors.New("GitHub sent something unexpected")
	}
	if rel.Draft || rel.Prerelease || !IsRelease(rel.Tag) {
		return nil, errors.New("the latest release isn't a full release")
	}
	// Only links into this repository's releases are ever opened.
	if !strings.HasPrefix(rel.Page, releaseURL) {
		rel.Page = releaseURL + "tag/" + rel.Tag
	}
	c.mu.Lock()
	c.etag = resp.Header.Get("ETag")
	c.mu.Unlock()
	return &rel, nil
}

// asset picks the download for this computer: the DMG on macOS, the MSI on Windows, the
// tarball on Linux. Falls back to the release page when there isn't one.
func (r *release) asset(goos, goarch string) (url, name string) {
	var suffix string
	switch goos {
	case "darwin":
		suffix = "-macos-universal.dmg"
	case "windows":
		suffix = "-windows-x64.msi"
		if goarch == "arm64" {
			suffix = "-windows-arm64.msi"
		}
	case "linux":
		suffix = "-linux-" + goarch + ".tar.gz"
	}
	want := "k8s-buddy-" + strings.TrimPrefix(r.Tag, "v") + suffix
	for _, a := range r.Assets {
		if suffix != "" && a.Name == want && strings.HasPrefix(a.URL, releaseURL+"download/") {
			return a.URL, a.Name
		}
	}
	return "", ""
}
