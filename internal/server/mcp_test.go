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
	"github.com/dlebee/k8s-buddy/internal/monitor"
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

// An answer over the cap stays valid JSON: rows drop off the end of its list, and it says how
// many and which arguments narrow it down (only ones the tool really has).
func TestMCPCapKeepsJSONValid(t *testing.T) {
	tool := mcpTool{Params: []mcpParam{{Name: "cluster"}, {Name: "limit"}}}
	rows := make([]map[string]any, 200)
	for i := range rows {
		rows[i] = map[string]any{"n": i, "text": strings.Repeat("é", 50)}
	}
	check := func(body any, field string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		out := capResult(string(raw), 4000, tool)
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil || len(out) > 4000 {
			t.Fatalf("capped answer: %d bytes, parse error %v", len(out), err)
		}
		kept := got[field].([]any)
		if got["truncated"] != true || int(got["omitted"].(float64))+len(kept) != 200 || len(kept) == 0 {
			t.Fatalf("kept %d, omitted %v, truncated %v", len(kept), got["omitted"], got["truncated"])
		}
		if kept[0].(map[string]any)["n"] != float64(0) {
			t.Fatal("the first rows weren't the ones kept")
		}
		hint := got["truncatedHint"].(string)
		if !strings.Contains(hint, "cluster, limit") || strings.Contains(hint, "namespace") {
			t.Fatalf("hint names the wrong arguments: %s", hint)
		}
	}
	check(map[string]any{"available": true, "clusters": []string{"a", "b"}, "rows": rows}, "rows")
	check(rows, "items")
	if out := capResult(`{"rows":[1,2]}`, 4000, tool); out != `{"rows":[1,2]}` {
		t.Fatalf("a small answer was changed: %s", out)
	}
	text := capResult(strings.Repeat("a line of describe output\n", 500), 1000, tool)
	if len(text) > 1200 || !strings.Contains(text, "cut") || strings.Contains(text[:900], "a line of describe outp\n") {
		t.Fatalf("text cut badly: %q", text[len(text)-150:])
	}
}

func TestMCPQuotaToolHasItsFilters(t *testing.T) {
	for _, tool := range mcpTools {
		if tool.Name != "get_quota_recommendations" {
			continue
		}
		q, err := tool.query(map[string]any{"namespace": "shop", "brief": true})
		if err != nil || q.Get("namespace") != "shop" || q.Get("brief") != "1" || q.Get("limit") != "40" || q.Get("verdict") != "risk,missing,under,over" {
			t.Fatalf("query %v, err %v", q, err)
		}
		if q, _ = tool.query(nil); q.Get("brief") != "1" {
			t.Fatalf("brief isn't the default: %v", q)
		}
		q, _ = tool.query(map[string]any{"verdict": "risk", "limit": float64(5), "brief": false})
		if q.Has("brief") {
			t.Fatalf("brief=false didn't turn brief off: %v", q)
		}
		if q.Get("verdict") != "risk" || q.Get("limit") != "5" {
			t.Fatalf("overrides lost: %v", q)
		}
		return
	}
	t.Fatal("tool not found")
}

// The quota filters keep the most urgent rows, count every verdict, and say what a limit left out.
func TestFilterOpt(t *testing.T) {
	row := func(ns, verdict string) monitor.OptRow {
		return monitor.OptRow{Cluster: "a", Namespace: ns, Kind: "Deployment", Workload: "api", Verdict: verdict,
			Findings: []monitor.OptFinding{{Level: "bad", Text: "long explanation"}}, Command: "kubectl set resources", HistoryName: "Deployment/api"}
	}
	rep := monitor.OptReport{Available: true, Rows: []monitor.OptRow{
		row("shop", "risk"), row("web", "risk"), row("shop", "under"), row("shop", "over"), row("shop", "over"), row("shop", "ok"), row("web", "ok")}}
	parse := func(v any) map[string]any {
		raw, _ := json.Marshal(v)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return out
	}
	got := parse(filterOpt(rep, []string{"shop"}, []string{"risk", "under", "over"}, 2, false, false))
	rows := got["rows"].([]any)
	if len(rows) != 2 || rows[0].(map[string]any)["verdict"] != "risk" || rows[1].(map[string]any)["verdict"] != "under" {
		t.Fatalf("rows: %v", rows)
	}
	if got["total"] != float64(5) || got["matching"] != float64(4) || got["omitted"] != float64(2) || got["truncated"] != true {
		t.Fatalf("counts: total %v matching %v omitted %v truncated %v", got["total"], got["matching"], got["omitted"], got["truncated"])
	}
	if by := got["byVerdict"].(map[string]any); by["ok"] != float64(1) || by["over"] != float64(2) || by["risk"] != float64(1) {
		t.Fatalf("byVerdict: %v", by)
	}
	if rows[0].(map[string]any)["findings"] == nil {
		t.Fatal("the full answer lost its findings")
	}
	brief := parse(filterOpt(rep, nil, []string{"risk"}, 0, true, false))
	rows = brief["rows"].([]any)
	first := rows[0].(map[string]any)
	if len(rows) != 2 || first["findings"] != nil || first["historyName"] != nil || first["command"] != "kubectl set resources" || brief["truncated"] != false {
		t.Fatalf("brief: %v", brief)
	}
}

// A default quota answer has to be small enough for an AI client to read directly (about
// 100 KB): 40 brief rows with realistic values stay far under it, and so do 40 full ones.
func TestQuotaAnswerFitsAnAgent(t *testing.T) {
	val := monitor.OptValue{}
	res := monitor.OptResource{Request: val, Limit: val, Median: 123456789, P95: 223456789, Max: 323456789}
	var rep monitor.OptReport
	for i := range 900 {
		rep.Rows = append(rep.Rows, monitor.OptRow{
			Cluster: "cluster-with-a-long-name-" + strings.Repeat("x", 20), Namespace: "namespace-" + strings.Repeat("y", 30),
			Kind: "Deployment", Workload: fmt.Sprintf("workload-%d-%s", i, strings.Repeat("z", 30)), Container: "container-" + strings.Repeat("c", 20),
			Verdict: "over", Confidence: "good", CPU: res, Mem: res, Command: strings.Repeat("kubectl set resources ", 12),
			Findings:    []monitor.OptFinding{{Level: "warn", Text: strings.Repeat("an explanation ", 30)}, {Level: "info", Text: strings.Repeat("more ", 40)}},
			HistoryKind: "workload", HistoryName: "Deployment/" + strings.Repeat("z", 40)})
	}
	for _, brief := range []bool{true, false} {
		raw, _ := json.Marshal(filterOpt(rep, nil, []string{"over"}, 40, brief, false))
		if len(raw) > mcpMaxResult {
			t.Errorf("brief=%v: %d KB, over the %d KB cap", brief, len(raw)>>10, mcpMaxResult>>10)
		}
		t.Logf("brief=%v: %d KB for 40 rows", brief, len(raw)>>10)
	}
}

// System workloads are left out unless asked for, and the answer says how many were.
func TestFilterOptHidesSystemWorkloads(t *testing.T) {
	rep := monitor.OptReport{Rows: []monitor.OptRow{
		{Namespace: "kube-system", Workload: "kube-proxy", Verdict: "over", System: true},
		{Namespace: "shop", Workload: "api", Verdict: "over"}}}
	raw, _ := json.Marshal(filterOpt(rep, nil, nil, 10, true, false))
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	if rows := got["rows"].([]any); len(rows) != 1 || rows[0].(map[string]any)["workload"] != "api" || got["systemHidden"] != float64(1) || got["total"] != float64(1) {
		t.Fatalf("default: %s", raw)
	}
	raw, _ = json.Marshal(filterOpt(rep, nil, nil, 10, true, true))
	_ = json.Unmarshal(raw, &got)
	if rows := got["rows"].([]any); len(rows) != 2 || rows[0].(map[string]any)["system"] != true || got["systemHidden"] != float64(0) {
		t.Fatalf("with system: %s", raw)
	}
}

// Under a limit the urgent verdicts don't crowd out the rest: each verdict keeps a few rows, the
// order stays most urgent first, and the answer says which verdicts were cut into.
func TestSpreadLimitKeepsEveryVerdict(t *testing.T) {
	var rep monitor.OptReport
	for _, v := range []struct {
		verdict string
		n       int
	}{{"risk", 30}, {"missing", 30}, {"under", 80}, {"over", 96}} {
		for i := range v.n {
			rep.Rows = append(rep.Rows, monitor.OptRow{Namespace: "shop", Workload: fmt.Sprintf("%s-%d", v.verdict, i), Verdict: v.verdict})
		}
	}
	raw, _ := json.Marshal(filterOpt(rep, nil, nil, 40, true, false))
	var got struct {
		Rows             []struct{ Verdict, Workload string }
		Omitted          int
		OmittedByVerdict map[string]int
	}
	_ = json.Unmarshal(raw, &got)
	count := map[string]int{}
	last := -1
	order := map[string]int{"risk": 0, "missing": 1, "under": 2, "over": 3}
	for _, r := range got.Rows {
		count[r.Verdict]++
		if order[r.Verdict] < last {
			t.Fatalf("rows out of order at %s", r.Workload)
		}
		last = order[r.Verdict]
	}
	if len(got.Rows) != 40 || count["under"] != 5 || count["over"] != 5 || count["risk"] != 25 || count["missing"] != 5 {
		t.Fatalf("kept %d: %v", len(got.Rows), count)
	}
	if got.Rows[0].Workload != "risk-0" || got.Omitted != 196 || got.OmittedByVerdict["over"] != 91 || got.OmittedByVerdict["risk"] != 5 {
		t.Fatalf("first %s, omitted %d, by verdict %v", got.Rows[0].Workload, got.Omitted, got.OmittedByVerdict)
	}
}

// A namespace that matches nothing is flagged instead of quietly answering "0 rows".
func TestFilterOptWarnsOnUnknownNamespace(t *testing.T) {
	rep := monitor.OptReport{Rows: []monitor.OptRow{
		{Namespace: "kube-system", Workload: "kube-proxy", Verdict: "over", System: true},
		{Namespace: "shop", Workload: "api", Verdict: "risk"}}}
	warn := func(ns string) []any {
		raw, _ := json.Marshal(filterOpt(rep, []string{ns}, nil, 10, true, false))
		var got map[string]any
		_ = json.Unmarshal(raw, &got)
		return got["warnings"].([]any)
	}
	// total and byVerdict follow the namespace too: a typo reads as nothing there, not as 0 of the fleet.
	raw, _ := json.Marshal(filterOpt(rep, []string{"shpo"}, nil, 10, true, false))
	var typo map[string]any
	_ = json.Unmarshal(raw, &typo)
	if typo["total"] != float64(0) || typo["matching"] != float64(0) || len(typo["byVerdict"].(map[string]any)) != 0 {
		t.Fatalf("unknown namespace still counts rows: %s", raw)
	}
	if w := warn("shop"); len(w) != 0 {
		t.Fatalf("a real namespace was flagged: %v", w)
	}
	if w := warn("shpo"); len(w) != 1 || !strings.Contains(w[0].(string), "check the spelling") {
		t.Fatalf("typo: %v", w)
	}
	if w := warn("kube-system"); len(w) != 1 || !strings.Contains(w[0].(string), "system=true") {
		t.Fatalf("system-only namespace: %v", w)
	}
}

// The counts share one scope: byVerdict splits total, and matching is the sum of the picked verdicts.
func TestFilterOptCountsAddUp(t *testing.T) {
	var rep monitor.OptReport
	for i, v := range []string{"risk", "missing", "under", "over", "over", "ok", "ok", "ok"} {
		rep.Rows = append(rep.Rows, monitor.OptRow{Namespace: "shop", Workload: fmt.Sprint("w", i), Verdict: v},
			monitor.OptRow{Namespace: "kube-system", Workload: fmt.Sprint("s", i), Verdict: v, System: true})
	}
	for _, system := range []bool{false, true} {
		raw, _ := json.Marshal(filterOpt(rep, nil, []string{"risk", "over"}, 0, true, system))
		var got struct {
			Total, Matching, SystemHidden int
			ByVerdict                     map[string]int
			Rows                          []any
		}
		_ = json.Unmarshal(raw, &got)
		sum := 0
		for _, n := range got.ByVerdict {
			sum += n
		}
		if sum != got.Total || got.Matching != got.ByVerdict["risk"]+got.ByVerdict["over"] || len(got.Rows) != got.Matching || got.Total+got.SystemHidden != 16 {
			t.Fatalf("system=%v: total %d, byVerdict %v (sum %d), matching %d, rows %d, hidden %d", system, got.Total, got.ByVerdict, sum, got.Matching, len(got.Rows), got.SystemHidden)
		}
	}
}
