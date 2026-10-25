package proxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nizarmah/tokenpool/internal/pool"
)

const admin = "tp-admin-0123456789"
const alice = "tp-alice-0123456789"

func jwt(claims map[string]any) string {
	enc := base64.RawURLEncoding
	payload, _ := json.Marshal(claims)
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString(payload) + ".sig"
}

// writeCodexLogin writes a Codex CLI auth.json whose access token expires
// at exp, and returns its path.
func writeCodexLogin(t *testing.T, access, refresh string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	doc, _ := json.Marshal(map[string]any{
		"OPENAI_API_KEY": nil,
		"tokens":         map[string]any{"id_token": "x.e30.y", "access_token": access, "refresh_token": refresh, "account_id": "acct-123"},
		"last_refresh":   time.Now().UTC().Format(time.RFC3339),
	})
	if err := os.WriteFile(path, doc, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// codexStream answers like the ChatGPT Codex backend: always a stream,
// labelled text/plain, with its usage headers.
func codexStream(w http.ResponseWriter, _ *http.Request, _ []byte) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Codex-Primary-Used-Percent", "20")
	h.Set("X-Codex-Primary-Window-Minutes", "300")
	h.Set("X-Codex-Primary-Reset-At", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	h.Set("X-Codex-Secondary-Used-Percent", "35")
	h.Set("X-Codex-Secondary-Window-Minutes", "10080")
	h.Set("X-Codex-Secondary-Reset-At", strconv.FormatInt(time.Now().Add(96*time.Hour).Unix(), 10))
	for _, event := range []string{
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5-codex","status":"in_progress","output":[]}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"from codex"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"from codex"}]}}`,
		`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5-codex","status":"completed","output":[],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`,
	} {
		var head struct{ Type string }
		_ = json.Unmarshal([]byte(event), &head)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", head.Type, event)
	}
}

// claudeWithQuota is a Claude subscription answer with its unified limits.
func claudeWithQuota(week float64) func(http.ResponseWriter, *http.Request, []byte) {
	return func(w http.ResponseWriter, r *http.Request, body []byte) {
		h := w.Header()
		h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.1")
		h.Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", strconv.FormatFloat(week, 'f', -1, 64))
		h.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(time.Now().Add(48*time.Hour).Unix(), 10))
		claudeMessage(w, r, body)
	}
}

func codexUp(url, path string, extra string) string {
	return fmt.Sprintf("  - {name: codex, url: %q, format: openai, auth: chatgpt, token_file: %q, "+
		"models: {\"gpt-*\": \"\", \"claude-*\": gpt-5-codex}%s}\n", url, path, extra)
}

func claudeNamed(name, url string) string {
	return fmt.Sprintf("  - {name: %s, url: %q, format: anthropic, token: sk-ant-%s, models: {\"claude-*\": \"\"}}\n", name, url, name)
}

func TestCodexLoginUpstream(t *testing.T) {
	codex := newFake(t, codexStream)
	access := jwt(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	path := writeCodexLogin(t, access, "rt-1")
	srv, _ := newProxy(t, poolYAML("", codexUp(codex.URL+"/backend-api/codex", path, "")), "")

	// A Responses client asking for a Codex model, without streaming.
	res, body := call(t, "POST", srv.URL+"/v1/responses", alice,
		`{"model":"gpt-5-codex","input":"hi","max_output_tokens":50,"temperature":0.3,"stream":false}`)
	if res.StatusCode != 200 || res.Header.Get("X-Tokenpool-Upstream") != "codex" {
		t.Fatalf("status %d body %s", res.StatusCode, body)
	}
	var out struct {
		ID     string `json:"id"`
		Output []struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.ID != "resp_1" || len(out.Output) != 1 ||
		out.Output[0].Content[0].Text != "from codex" {
		t.Errorf("collected response = %s (%v)", body, err)
	}

	c := codex.calls()[0]
	if c.path != "/backend-api/codex/responses" {
		t.Errorf("path = %s", c.path)
	}
	if c.header.Get("Authorization") != "Bearer "+access || c.header.Get("Chatgpt-Account-Id") != "acct-123" ||
		c.header.Get("Originator") != "codex_cli_rs" || !strings.HasPrefix(c.header.Get("User-Agent"), "codex_cli_rs/") ||
		c.header.Get("Accept") != "text/event-stream" || c.header.Get("Session-Id") == "" {
		t.Errorf("codex headers = %v", c.header)
	}
	if c.body["stream"] != true || c.body["store"] != false || c.body["max_output_tokens"] != nil || c.body["temperature"] != nil {
		t.Errorf("codex body = %s", c.raw)
	}
	if input, ok := c.body["input"].([]any); !ok || len(input) != 1 {
		t.Errorf("string input not sent as a list: %s", c.raw)
	}
	// A streaming Responses client gets the stream as an event stream.
	res, body = call(t, "POST", srv.URL+"/v1/responses", alice, `{"model":"gpt-5-codex","input":"hi","stream":true}`)
	if res.Header.Get("Content-Type") != "text/event-stream" || !strings.Contains(body, "response.completed") {
		t.Errorf("stream: %s %s", res.Header.Get("Content-Type"), body)
	}

	// Claude Code, unpinned, reaches Codex through the claude-* mapping.
	res, body = call(t, "POST", srv.URL+"/v1/messages", alice, messagesBody)
	if res.StatusCode != 200 || !strings.Contains(body, "event: message_start") || !strings.Contains(body, "from codex") {
		t.Errorf("anthropic stream: %d %s", res.StatusCode, body)
	}
	if got := codex.calls()[2].body["model"]; got != "gpt-5-codex" {
		t.Errorf("mapped model = %v", got)
	}
	// And without streaming, as a Messages API answer.
	res, body = call(t, "POST", srv.URL+"/v1/messages", alice,
		`{"model":"claude-sonnet-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if res.StatusCode != 200 || !strings.Contains(body, `"type":"message"`) || !strings.Contains(body, "from codex") {
		t.Errorf("anthropic message: %d %s", res.StatusCode, body)
	}

	// Its usage headers show up in the admin API.
	_, body = call(t, "GET", srv.URL+"/admin/upstreams/codex", admin, "")
	var st pool.Status
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if st.Auth != "chatgpt" || st.Quota == nil || st.Quota.RemainingPercent == nil || *st.Quota.RemainingPercent != 65 ||
		st.Quota.Window != "codex.secondary" || st.Quota.ResetsAt == nil || len(st.Quota.Windows) != 2 {
		t.Errorf("codex status = %s", body)
	}
	if strings.Contains(body, access) {
		t.Error("access token leaked into the admin API")
	}
}

func TestCodexRefreshesARejectedToken(t *testing.T) {
	var refreshes atomic.Int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["refresh_token"] != "rt-1" || body["grant_type"] != "refresh_token" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "fresh-access", "refresh_token": "rt-2"})
	}))
	defer tokenSrv.Close()
	prev := pool.ChatGPTTokenURL
	pool.ChatGPTTokenURL = tokenSrv.URL
	defer func() { pool.ChatGPTTokenURL = prev }()

	codex := newFake(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if r.Header.Get("Authorization") != "Bearer fresh-access" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"token expired"}}`)
			return
		}
		codexStream(w, r, body)
	})
	path := writeCodexLogin(t, jwt(map[string]any{"exp": time.Now().Add(time.Hour).Unix()}), "rt-1")
	srv, p := newProxy(t, poolYAML("", codexUp(codex.URL, path, "")), "")

	res, body := call(t, "POST", srv.URL+"/v1/responses", alice, `{"model":"gpt-5-codex","input":"hi","stream":true}`)
	if res.StatusCode != 200 || !strings.Contains(body, "from codex") {
		t.Fatalf("status %d body %s", res.StatusCode, body)
	}
	if refreshes.Load() != 1 || len(codex.calls()) != 2 {
		t.Errorf("refreshes %d, codex calls %d", refreshes.Load(), len(codex.calls()))
	}
	if st, _ := p.Get("codex"); st.State != "available" {
		t.Errorf("codex was benched: %+v", st)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "fresh-access") || !strings.Contains(string(data), "rt-2") {
		t.Errorf("auth.json not updated: %s", data)
	}
}

func TestCodexUsageLimitBenchesUntilReset(t *testing.T) {
	codex := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("X-Codex-Primary-Used-Percent", "100")
		w.Header().Set("X-Codex-Primary-Reset-At", strconv.FormatInt(time.Now().Add(90*time.Minute).Unix(), 10))
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`)
	})
	claude := newFake(t, claudeMessage)
	path := writeCodexLogin(t, jwt(map[string]any{"exp": time.Now().Add(time.Hour).Unix()}), "rt")
	srv, p := newProxy(t, poolYAML("", codexUp(codex.URL, path, ", priority: -1"), claudeNamed("claude", claude.URL)), "")

	res, body := call(t, "POST", srv.URL+"/v1/messages", alice,
		`{"model":"claude-sonnet-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if res.StatusCode != 200 || res.Header.Get("X-Tokenpool-Upstream") != "claude" {
		t.Fatalf("status %d upstream %q body %s", res.StatusCode, res.Header.Get("X-Tokenpool-Upstream"), body)
	}
	st, _ := p.Get("codex")
	if st.State != "cooling" || st.CooldownUntil == nil || time.Until(*st.CooldownUntil) < 85*time.Minute {
		t.Errorf("codex = %+v", st)
	}
}

func TestPinningAndAliases(t *testing.T) {
	codex := newFake(t, codexStream)
	claude := newFake(t, claudeMessage)
	path := writeCodexLogin(t, jwt(map[string]any{"exp": time.Now().Add(time.Hour).Unix()}), "rt")
	yaml := poolYAML("model_aliases: {best-code: gpt-5-codex}\n",
		codexUp(codex.URL, path, ", priority: -1"), claudeNamed("claude", claude.URL))
	srv, p := newProxy(t, yaml, "")
	plain := `{"model":"claude-sonnet-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`

	// Unpinned: codex has the lower priority, so it takes Claude requests.
	res, _ := call(t, "POST", srv.URL+"/v1/messages", alice, plain)
	if got := res.Header.Get("X-Tokenpool-Upstream"); got != "codex" {
		t.Errorf("unpinned went to %s", got)
	}
	// Pinned: only an upstream that serves claude-sonnet-5 itself.
	res, _ = call(t, "POST", srv.URL+"/v1/messages", alice, plain, "X-Tokenpool-Pin", "true")
	if got := res.Header.Get("X-Tokenpool-Upstream"); got != "claude" {
		t.Errorf("pinned went to %s", got)
	}
	if h := claude.calls()[0].header; h.Get("X-Tokenpool-Pin") != "" {
		t.Error("the pin header was forwarded upstream")
	}
	// Pinned with the native upstream out: no translation, a clear error.
	if _, err := p.SetDisabled("claude", true); err != nil {
		t.Fatal(err)
	}
	res, body := call(t, "POST", srv.URL+"/v1/messages", alice, plain, "X-Tokenpool-Pin", "true")
	if res.StatusCode != 503 || !strings.Contains(body, "claude: disabled") {
		t.Errorf("pinned, native paused: %d %s", res.StatusCode, body)
	}
	res, body = call(t, "POST", srv.URL+"/v1/messages", alice, strings.Replace(plain, "claude-sonnet-5", "kimi-k2", 1),
		"X-Tokenpool-Pin", "1")
	if res.StatusCode != 404 || !strings.Contains(body, "pinned") {
		t.Errorf("pinned, nobody serves it: %d %s", res.StatusCode, body)
	}

	// An alias names a model; the upstream gets the real name.
	res, _ = call(t, "POST", srv.URL+"/v1/responses", alice, `{"model":"best-code","input":"hi"}`)
	if res.StatusCode != 200 || res.Header.Get("X-Tokenpool-Upstream") != "codex" {
		t.Errorf("alias: %d %s", res.StatusCode, res.Header.Get("X-Tokenpool-Upstream"))
	}
	calls := codex.calls()
	if got := calls[len(calls)-1].body["model"]; got != "gpt-5-codex" {
		t.Errorf("alias sent model %v", got)
	}
	_, body = call(t, "GET", srv.URL+"/v1/models", alice, "")
	if !strings.Contains(body, `"best-code"`) {
		t.Errorf("models list lacks the alias: %s", body)
	}
}

func TestPinModelsConfig(t *testing.T) {
	codex := newFake(t, codexStream)
	claude := newFake(t, claudeMessage)
	path := writeCodexLogin(t, jwt(map[string]any{"exp": time.Now().Add(time.Hour).Unix()}), "rt")
	srv, _ := newProxy(t, poolYAML("pin_models: true\n", codexUp(codex.URL, path, ", priority: -1"), claudeNamed("claude", claude.URL)), "")
	plain := `{"model":"claude-sonnet-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	res, _ := call(t, "POST", srv.URL+"/v1/messages", alice, plain)
	if got := res.Header.Get("X-Tokenpool-Upstream"); got != "claude" {
		t.Errorf("pin_models: went to %s", got)
	}
	res, _ = call(t, "POST", srv.URL+"/v1/messages", alice, plain, "X-Tokenpool-Pin", "false")
	if got := res.Header.Get("X-Tokenpool-Upstream"); got != "codex" {
		t.Errorf("unpinned by header: went to %s", got)
	}
}

func TestAdminRoutingControls(t *testing.T) {
	codex := newFake(t, codexStream)
	busy := newFake(t, claudeWithQuota(0.98))
	fresh := newFake(t, claudeWithQuota(0.25))
	path := writeCodexLogin(t, jwt(map[string]any{"exp": time.Now().Add(time.Hour).Unix()}), "rt")
	file := filepath.Join(t.TempDir(), "pool.json")
	yaml := poolYAML("", claudeNamed("claude-busy", busy.URL), claudeNamed("claude-fresh", fresh.URL), codexUp(codex.URL, path, ""))
	srv, _ := newProxy(t, yaml, file)
	plain := `{"model":"claude-sonnet-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	served := func() string {
		t.Helper()
		res, body := call(t, "POST", srv.URL+"/v1/messages", alice, plain)
		if res.StatusCode != 200 {
			t.Fatalf("status %d body %s", res.StatusCode, body)
		}
		return res.Header.Get("X-Tokenpool-Upstream")
	}
	adminCall := func(method, path, body string) string {
		t.Helper()
		res, out := call(t, method, srv.URL+path, admin, body)
		if res.StatusCode != 200 {
			t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, out)
		}
		return out
	}

	// Priority (the default strategy): config order among equals, natives first.
	if got := served(); got != "claude-busy" {
		t.Fatalf("priority served %s", got)
	}
	served() // let every Claude upstream report its quota
	adminCall("POST", "/admin/upstreams/claude-busy/pause", "")
	served()
	adminCall("POST", "/admin/upstreams/claude-busy/resume", "")

	// most_quota: claude-busy reported 2% left this week, claude-fresh 75%,
	// codex has not answered yet and counts as full.
	out := adminCall("PUT", "/admin/strategy", `{"strategy":"most-quota"}`)
	var status struct {
		pool.Summary
		PinModels bool `json:"pin_models"`
	}
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatal(err)
	}
	if status.Strategy != "most_quota" || status.First != "codex" || len(status.Upstreams) != 3 ||
		status.Upstreams[1].Name != "claude-fresh" || status.Upstreams[2].Name != "claude-busy" ||
		*status.Upstreams[2].RemainingPercent != 2 || status.Upstreams[2].Window != "7d" {
		t.Errorf("status = %s", out)
	}
	if got := served(); got != "codex" {
		t.Errorf("most_quota served %s", got)
	}
	// Codex reported 65% left: claude-fresh, with 75%, is now ahead.
	if got := served(); got != "claude-fresh" {
		t.Errorf("most_quota after codex reported served %s", got)
	}

	// Default: one upstream takes everything it can.
	adminCall("POST", "/admin/upstreams/claude-busy/default", "")
	if got := served(); got != "claude-busy" {
		t.Errorf("default served %s", got)
	}
	// Priority changes go through the same API.
	adminCall("DELETE", "/admin/default", "")
	adminCall("POST", "/admin/upstreams/codex/priority", `{"priority":-5}`)
	if got := served(); got != "codex" {
		t.Errorf("after priority change served %s", got)
	}
	res, _ := call(t, "POST", srv.URL+"/admin/upstreams/codex/priority", admin, `{}`)
	if res.StatusCode != 400 {
		t.Errorf("priority without a value: %d", res.StatusCode)
	}
	res, _ = call(t, "PUT", srv.URL+"/admin/strategy", admin, `{"strategy":"random"}`)
	if res.StatusCode != 400 {
		t.Errorf("bad strategy: %d", res.StatusCode)
	}
	res, _ = call(t, "POST", srv.URL+"/admin/upstreams/nope/default", admin, "")
	if res.StatusCode != 404 {
		t.Errorf("unknown default: %d", res.StatusCode)
	}
	res, _ = call(t, "GET", srv.URL+"/admin/status", alice, "")
	if res.StatusCode != 401 {
		t.Errorf("client key read the status: %d", res.StatusCode)
	}

	// Every change survives a restart.
	srv2, _ := newProxy(t, yaml, file)
	res, body := call(t, "GET", srv2.URL+"/admin/status", admin, "")
	if res.StatusCode != 200 || !strings.Contains(body, `"strategy":"priority"`) || !strings.Contains(body, `"first":"codex"`) {
		t.Errorf("after restart: %s", body)
	}
}
