package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dlebee/k8s-buddy/internal/config"
)

const testMCPKey = "test-key-0123456789abcdef"

func unusedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// newSettingsServer is a server with real settings in a temporary folder (and no clusters).
func newSettingsServer(t *testing.T) *Server {
	t.Helper()
	st, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, st, nil)
	ln, err := s.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	s.Handler()
	return s
}

// startMCP turns the MCP server on for a test server and returns its URL.
func startMCP(t *testing.T) (*Server, string) {
	t.Helper()
	s := newSettingsServer(t)
	port := unusedPort(t)
	if err := s.settings.Update(func(st *config.Settings) { st.MCP = config.MCP{Enabled: true, Port: port, Key: testMCPKey} }); err != nil {
		t.Fatal(err)
	}
	s.StartMCP()
	t.Cleanup(s.StopMCP)
	if st := s.MCPStatus(); !st.Running {
		t.Fatalf("not running: %+v", st)
	}
	return s, fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
}

func mcpPost(t *testing.T, url, auth, body string, hdr ...string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			req.Host = hdr[i+1]
		} else {
			req.Header.Set(hdr[i], hdr[i+1])
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestMCPOffByDefault(t *testing.T) {
	s := newSettingsServer(t)
	s.StartMCP()
	t.Cleanup(s.StopMCP)
	if st := s.MCPStatus(); st.Enabled || st.Running {
		t.Fatalf("MCP runs without being turned on: %+v", st)
	}
	if config.Defaults().MCP.Enabled {
		t.Fatal("MCP is on by default")
	}
}

// The key is the last word of the Authorization header, whatever the prefix. Without it, with a
// wrong one, from another site's page or under another host name, nothing answers.
func TestMCPGuard(t *testing.T) {
	_, url := startMCP(t)
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	for _, auth := range []string{"Bearer " + testMCPKey, "ApiKey " + testMCPKey, "anything at all " + testMCPKey, testMCPKey} {
		if code, _ := mcpPost(t, url, auth, ping); code != http.StatusOK {
			t.Errorf("Authorization %q: got %d, want 200", auth, code)
		}
	}
	for _, auth := range []string{"", "Bearer", "Bearer wrong-key-0123456789abcdef", "Bearer " + testMCPKey + "x", testMCPKey + " Bearer"} {
		if code, _ := mcpPost(t, url, auth, ping); code != http.StatusUnauthorized {
			t.Errorf("Authorization %q: got %d, want 401", auth, code)
		}
	}
	ok := "Bearer " + testMCPKey
	if code, _ := mcpPost(t, url, ok, ping, "Origin", "https://evil.example.com"); code != http.StatusForbidden {
		t.Errorf("another site's page: got %d, want 403", code)
	}
	if code, _ := mcpPost(t, url, ok, ping, "Origin", "http://localhost:6274"); code != http.StatusOK {
		t.Errorf("a local page: got %d, want 200", code)
	}
	if code, _ := mcpPost(t, url, ok, ping, "Host", "evil.example.com"); code != http.StatusForbidden {
		t.Errorf("another host name (DNS rebinding): got %d, want 403", code)
	}
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", ok)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: %v %v, want 405", resp, err)
	}
}

func TestMCPProtocol(t *testing.T) {
	_, url := startMCP(t)
	auth := "Bearer " + testMCPKey
	_, init := mcpPost(t, url, auth, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	res, _ := init["result"].(map[string]any)
	if res["protocolVersion"] != "2025-03-26" || res["serverInfo"].(map[string]any)["name"] != "k8s-buddy" {
		t.Fatalf("initialize: %v", init)
	}
	_, init = mcpPost(t, url, auth, `{"jsonrpc":"2.0","id":"a","method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if v := init["result"].(map[string]any)["protocolVersion"]; v != mcpVersions[0] || init["id"] != "a" {
		t.Fatalf("unknown version: got %v, id %v", v, init["id"])
	}
	if code, _ := mcpPost(t, url, auth, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != http.StatusAccepted {
		t.Fatalf("notification: got %d, want 202", code)
	}
	_, list := mcpPost(t, url, auth, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) != len(mcpTools) {
		t.Fatalf("tools/list: %d tools, want %d", len(tools), len(mcpTools))
	}
	_, call := mcpPost(t, url, auth, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_kinds","arguments":{}}}`)
	cr := call["result"].(map[string]any)
	text := cr["content"].([]any)[0].(map[string]any)["text"].(string)
	if cr["isError"] != false || !strings.Contains(text, `"pods"`) {
		t.Fatalf("list_kinds: isError %v, text %.200s", cr["isError"], text)
	}
	_, call = mcpPost(t, url, auth, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_resource","arguments":{"cluster":"a"}}}`)
	if call["result"].(map[string]any)["isError"] != true {
		t.Fatalf("missing required argument wasn't an error: %v", call)
	}
	_, call = mcpPost(t, url, auth, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"list_kinds","arguments":{"t":"x"}}}`)
	if call["result"].(map[string]any)["isError"] != true {
		t.Fatalf("undeclared argument wasn't refused: %v", call)
	}
	_, bad := mcpPost(t, url, auth, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"reveal_secret"}}`)
	if bad["error"] == nil {
		t.Fatalf("unknown tool: %v", bad)
	}
	_, bad = mcpPost(t, url, auth, `{"jsonrpc":"2.0","id":7,"method":"resources/list"}`)
	if e, _ := bad["error"].(map[string]any); e == nil || e["code"] != float64(-32601) {
		t.Fatalf("unknown method: %v", bad)
	}
}

// Every tool is a GET on a read API: nothing that changes or reveals something is reachable, and
// settings (which hold the key) aren't either.
func TestMCPToolsAreReadOnly(t *testing.T) {
	s := newSettingsServer(t)
	for _, tool := range mcpTools {
		for _, banned := range []string{"settings", "secret", "helm", "exec", "shell", "portforward", "action", "login", "mute", "ack", "open", "reload", "session", "stream", "logs"} {
			if strings.Contains(tool.Path, banned) {
				t.Errorf("%s reaches %s", tool.Name, tool.Path)
			}
		}
		req, _ := http.NewRequest(http.MethodGet, "http://k8s-buddy"+tool.Path, nil)
		_, pattern := s.api.(*http.ServeMux).Handler(req)
		if pattern != "GET "+tool.Path {
			t.Errorf("%s: %s is served by %q, want a GET route of its own", tool.Name, tool.Path, pattern)
		}
	}
}

// Turning it off or moving the port takes effect right away; a changed key too.
func TestMCPFollowsSettings(t *testing.T) {
	s, url := startMCP(t)
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	const other = "another-key-0123456789abcdef"
	_ = s.settings.Update(func(st *config.Settings) { st.MCP.Key = other })
	if code, _ := mcpPost(t, url, "Bearer "+testMCPKey, ping); code != http.StatusUnauthorized {
		t.Fatalf("old key still works: %d", code)
	}
	if code, _ := mcpPost(t, url, "Bearer "+other, ping); code != http.StatusOK {
		t.Fatalf("new key: %d", code)
	}
	_ = s.settings.Update(func(st *config.Settings) { st.MCP.Enabled = false })
	if st := s.MCPStatus(); st.Running {
		t.Fatal("still running after being turned off")
	}
	if _, err := http.Post(url, "application/json", strings.NewReader(ping)); err == nil {
		t.Fatal("still answering after being turned off")
	}
}

func TestMCPKeyRules(t *testing.T) {
	for k, want := range map[string]bool{
		testMCPKey: true, config.NewMCPKey(): true,
		"": false, "short": false, "has a space in it 123456": false, "tab\tin-it-0123456789abc": false, strings.Repeat("x", 300): false,
	} {
		if got := config.ValidMCPKey(k); got != want {
			t.Errorf("ValidMCPKey(%q) = %v, want %v", k, got, want)
		}
	}
	if mcpAuthorized("Bearer ", "") || mcpAuthorized("", "") {
		t.Fatal("an empty key authorizes")
	}
}

// Guessing the key is slowed: after 20 wrong keys nothing is checked for a moment, then one try
// every 3 seconds. A right key doesn't use up the allowance.
func TestMCPGuessLimit(t *testing.T) {
	_, url := startMCP(t)
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	for range 50 {
		if code, _ := mcpPost(t, url, "Bearer "+testMCPKey, ping); code != http.StatusOK {
			t.Fatalf("right key: %d", code)
		}
	}
	for i := range mcpGuessBurst {
		if code, _ := mcpPost(t, url, fmt.Sprintf("Bearer wrong-key-%020d", i), ping); code != http.StatusUnauthorized {
			t.Fatalf("wrong key %d: got %d, want 401", i, code)
		}
	}
	if code, _ := mcpPost(t, url, "Bearer wrong-key-0123456789abcdef", ping); code != http.StatusTooManyRequests {
		t.Fatalf("after %d wrong keys: got %d, want 429", mcpGuessBurst, code)
	}
	var g guessLimit
	now := time.Now()
	g.ok(now)
	for range mcpGuessBurst {
		g.spend()
	}
	if g.ok(now.Add(time.Second)) || !g.ok(now.Add(mcpGuessEvery+time.Second)) {
		t.Fatal("the allowance doesn't refill one try per 3 seconds")
	}
}
