package monitor

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// When the account may not list something (Helm's secrets cluster-wide, say), client-go's
// informers keep retrying the list every few seconds forever, and every attempt lands in the
// cluster's audit log. denialCache remembers a refused list for an hour and answers the
// retries itself with the same refusal, so the API server sees one request per hour instead.
// Permissions can change, so after the hour the next attempt goes through again.

const denialTTL = time.Hour

type denial struct {
	until  time.Time
	header http.Header
	body   []byte
}

// denialCache is shared by every client made from one cluster's config; each client's transport
// is wrapped by its own denialRT pointing at it.
type denialCache struct {
	now  func() time.Time
	mu   sync.Mutex
	seen map[string]denial
}

func newDenialCache() *denialCache {
	return &denialCache{now: time.Now, seen: map[string]denial{}}
}

func (d *denialCache) wrap(next http.RoundTripper) http.RoundTripper {
	return &denialRT{next: next, d: d}
}

type denialRT struct {
	next http.RoundTripper
	d    *denialCache
}

// denialKey identifies a list or watch of one collection with one set of selectors. Only
// requests shaped like an informer's (or any list) are remembered; reading a single object by
// name always goes through.
func denialKey(r *http.Request) (string, bool) {
	if r.Method != http.MethodGet {
		return "", false
	}
	q := r.URL.Query()
	listish := q.Has("watch") || q.Has("limit") || q.Has("resourceVersion") || q.Has("sendInitialEvents") ||
		q.Has("labelSelector") || q.Has("fieldSelector")
	if !listish {
		return "", false
	}
	return r.URL.Host + r.URL.Path + "?l=" + q.Get("labelSelector") + "&f=" + q.Get("fieldSelector"), true
}

func (t *denialRT) RoundTrip(r *http.Request) (*http.Response, error) {
	d := t.d
	key, ok := denialKey(r)
	if !ok {
		return t.next.RoundTrip(r)
	}
	d.mu.Lock()
	den, cached := d.seen[key]
	if cached && d.now().After(den.until) {
		delete(d.seen, key)
		cached = false
	}
	d.mu.Unlock()
	if cached {
		return &http.Response{
			StatusCode:    http.StatusForbidden,
			Status:        "403 Forbidden",
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        den.header.Clone(),
			Body:          io.NopCloser(bytes.NewReader(den.body)),
			ContentLength: int64(len(den.body)),
			Request:       r,
		}, nil
	}
	resp, err := t.next.RoundTrip(r)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		return resp, err
	}
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if rerr != nil {
		return nil, rerr
	}
	until := d.now().Add(denialTTL)
	body = noteRetry(body, until)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Del("Content-Length")
	d.mu.Lock()
	d.seen[key] = denial{until: until, header: resp.Header.Clone(), body: body}
	d.mu.Unlock()
	return resp, nil
}

// noteRetry adds when we'll ask again to the API server's message, so the UI can say so.
func noteRetry(body []byte, until time.Time) []byte {
	var st map[string]any
	if json.Unmarshal(body, &st) != nil {
		return body
	}
	msg, _ := st["message"].(string)
	st["message"] = msg + " (k8s-buddy asks again at " + until.Local().Format("15:04") + ")"
	out, err := json.Marshal(st)
	if err != nil {
		return body
	}
	return out
}
