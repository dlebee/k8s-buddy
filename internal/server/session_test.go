package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s := New(nil, nil, nil)
	ln, err := s.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	return s, s.Handler()
}

func redeemCode(h http.Handler, s *Server, code, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/session", strings.NewReader(`{"code":"`+code+`"}`))
	r.Host = s.addr
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestBrowserURLCarriesOneTimeCodeNotToken(t *testing.T) {
	s, h := newTestServer(t)
	u, err := url.Parse(s.BrowserURL("overview"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(u.String(), s.token) || u.Query().Get("t") != "" {
		t.Fatalf("browser URL leaks the token: %s", u)
	}
	code := u.Query().Get("c")
	if len(code) != 32 {
		t.Fatalf("code %q", code)
	}

	if w := redeemCode(h, s, code, "https://evil.example"); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin redeem: %d", w.Code)
	}
	w := redeemCode(h, s, code, "http://"+s.addr)
	var got struct{ Token string }
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Token != s.token {
		t.Fatalf("redeem: %d %s", w.Code, w.Body)
	}
	if w := redeemCode(h, s, code, ""); w.Code != http.StatusForbidden {
		t.Fatalf("a code works only once, second use: %d", w.Code)
	}
	if w := redeemCode(h, s, "", ""); w.Code != http.StatusForbidden {
		t.Fatalf("empty code: %d", w.Code)
	}
}

func TestBrowserCodeExpires(t *testing.T) {
	s, h := newTestServer(t)
	code := strings.TrimPrefix(s.BrowserURL("overview"), "http://"+s.addr+"/overview?c=")
	s.codesMu.Lock()
	s.codes[code] = time.Now().Add(-time.Second)
	s.codesMu.Unlock()
	if w := redeemCode(h, s, code, ""); w.Code != http.StatusForbidden {
		t.Fatalf("expired code: %d", w.Code)
	}
}

func TestGuard(t *testing.T) {
	s, h := newTestServer(t)
	do := func(method, path, host, token string) int {
		r := httptest.NewRequest(method, path, nil)
		r.Host = host
		if token != "" {
			r.Header.Set("X-Buddy-Token", token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := do("GET", "/api/kinds", s.addr, ""); c != http.StatusForbidden {
		t.Errorf("no token: %d", c)
	}
	if c := do("GET", "/api/kinds", s.addr, "wrong"); c != http.StatusForbidden {
		t.Errorf("wrong token: %d", c)
	}
	if c := do("GET", "/api/kinds", "buddy.test:"+strings.Split(s.addr, ":")[1], s.token); c != http.StatusForbidden {
		t.Errorf("foreign Host (DNS rebinding): %d", c)
	}
	if c := do("GET", "/api/kinds", s.addr, s.token); c != http.StatusOK {
		t.Errorf("token: %d", c)
	}
	if c := do("GET", "/api/session", s.addr, ""); c != http.StatusForbidden {
		t.Errorf("only POST /api/session is public: %d", c)
	}
}
