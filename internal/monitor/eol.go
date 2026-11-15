package monitor

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/dlebee/k8s-buddy/internal/config"
)

// EOL data from endoflife.date. We cache for a day on disk so restarts don't refetch,
// and EOL dates move on month scales anyway.

const (
	eolURL       = "https://endoflife.date/api/kubernetes.json"
	eolFreshFor  = 24 * time.Hour
	eolFetchFail = 6 * time.Hour // cooldown after a failed fetch
)

type eolEntry struct {
	Minor       string    `json:"cycle"`
	EOL         time.Time `json:"eol"`
	LatestPatch string    `json:"latest"`
	ReleaseDate time.Time `json:"releaseDate"`
}

type eolCache struct {
	FetchedAt time.Time  `json:"fetchedAt"`
	Entries   []eolEntry `json:"entries"`
	byMinor   map[string]eolEntry
}

var (
	eolMu       sync.Mutex
	eolData     *eolCache
	eolLastTry  time.Time
	eolFetching bool
)

// Raw endoflife.date entries use string dates; we reshape to time.Time.
type rawEOLEntry struct {
	Cycle       string `json:"cycle"`
	EOL         any    `json:"eol"`
	Latest      string `json:"latest"`
	ReleaseDate string `json:"releaseDate"`
}

func eolFilePath() string {
	d, err := config.Dir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "k8s-eol.json")
}

func loadEOLFromDisk() *eolCache {
	p := eolFilePath()
	if p == "" {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var c eolCache
	if err := json.Unmarshal(b, &c); err != nil {
		return nil
	}
	c.byMinor = make(map[string]eolEntry, len(c.Entries))
	for _, e := range c.Entries {
		c.byMinor[e.Minor] = e
	}
	return &c
}

func saveEOLToDisk(c *eolCache) {
	p := eolFilePath()
	if p == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	_ = os.WriteFile(p, b, 0o600)
}

func fetchEOL() (*eolCache, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(eolURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("endoflife.date: status %d", resp.StatusCode)
	}
	var raw []rawEOLEntry
	// Third-party response: cap what we read (the real list is a few KB).
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&raw); err != nil {
		return nil, err
	}
	c := &eolCache{FetchedAt: time.Now(), byMinor: map[string]eolEntry{}}
	for _, r := range raw {
		e := eolEntry{Minor: r.Cycle, LatestPatch: r.Latest}
		if s, ok := r.EOL.(string); ok {
			if t, err := time.Parse("2006-01-02", s); err == nil {
				e.EOL = t
			}
		}
		if r.ReleaseDate != "" {
			if t, err := time.Parse("2006-01-02", r.ReleaseDate); err == nil {
				e.ReleaseDate = t
			}
		}
		if e.EOL.IsZero() {
			continue
		}
		c.Entries = append(c.Entries, e)
		c.byMinor[e.Minor] = e
	}
	return c, nil
}

// ensureEOL returns a usable cache (possibly stale), and triggers an async refresh when stale.
func ensureEOL() *eolCache {
	eolMu.Lock()
	if eolData == nil {
		eolData = loadEOLFromDisk()
	}
	fresh := eolData != nil && time.Since(eolData.FetchedAt) < eolFreshFor
	shouldFetch := !fresh && !eolFetching && time.Since(eolLastTry) > eolFetchFail
	if shouldFetch {
		eolFetching = true
		eolLastTry = time.Now()
	}
	cur := eolData
	eolMu.Unlock()

	if !shouldFetch {
		return cur
	}
	go func() {
		c, err := fetchEOL()
		eolMu.Lock()
		eolFetching = false
		if err == nil && c != nil {
			eolData = c
			saveEOLToDisk(c)
		}
		eolMu.Unlock()
	}()
	return cur
}

var minorRe = regexp.MustCompile(`v?(\d+)\.(\d+)`)

func minorOf(gitVersion string) string {
	m := minorRe.FindStringSubmatch(gitVersion)
	if m == nil {
		return ""
	}
	return m[1] + "." + m[2]
}

// lookupEOL returns the EOL entry for a server gitVersion (e.g. "v1.28.5"), or ok=false.
func lookupEOL(gitVersion string) (eolEntry, bool) {
	minor := minorOf(gitVersion)
	if minor == "" {
		return eolEntry{}, false
	}
	c := ensureEOL()
	if c == nil {
		return eolEntry{}, false
	}
	e, ok := c.byMinor[minor]
	return e, ok
}
