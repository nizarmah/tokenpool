package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nizarmah/tokenpool/internal/config"
	"github.com/nizarmah/tokenpool/internal/pool"
)

// fake is an upstream that records what it receives and answers with reply.
type fake struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recorded
	reply    func(w http.ResponseWriter, r *http.Request, body []byte)
}

type recorded struct {
	path   string
	header http.Header
	body   map[string]any
	raw    string
}

func newFake(t *testing.T, reply func(w http.ResponseWriter, r *http.Request, body []byte)) *fake {
	f := &fake{reply: reply}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec := recorded{path: r.URL.RequestURI(), header: r.Header.Clone(), raw: string(body)}
		_ = json.Unmarshal(body, &rec.body)
		f.mu.Lock()
		f.requests = append(f.requests, rec)
		reply := f.reply
		f.mu.Unlock()
		reply(w, r, body)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fake) setReply(reply func(w http.ResponseWriter, r *http.Request, body []byte)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply = reply
}

func (f *fake) calls() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.requests...)
}

func newProxy(t *testing.T, yaml string, poolFile string) (*httptest.Server, *pool.Pool) {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	p, err := pool.New(cfg.Strategy, cfg.Upstreams, poolFile)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(New(cfg, p, log).Handler())
	t.Cleanup(srv.Close)
	return srv, p
}

func call(t *testing.T, method, url, key, body string, headers ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	for i := 0; i < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func rateLimited(w http.ResponseWriter, _ *http.Request, _ []byte) {
	w.Header().Set("Retry-After", "120")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"Number of requests has exceeded your rate limit"}}`)
}

func claudeMessage(w http.ResponseWriter, _ *http.Request, _ []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Request-Id", "req_123")
	io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5",
		"content":[{"type":"text","text":"from claude"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":3,"output_tokens":2}}`)
}

func grokStream(w http.ResponseWriter, _ *http.Request, _ []byte) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, chunk := range []string{
		`{"id":"g1","model":"grok-4","choices":[{"index":0,"delta":{"role":"assistant","content":"from "}}]}`,
		`{"id":"g1","model":"grok-4","choices":[{"index":0,"delta":{"content":"grok"},"finish_reason":"stop"}]}`,
		`{"id":"g1","model":"grok-4","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		`[DONE]`,
	} {
		fmt.Fprintf(w, "data: %s\n\n", chunk)
		w.(http.Flusher).Flush()
	}
}

const messagesBody = `{"model":"claude-sonnet-5","max_tokens":100,"stream":true,
	"messages":[{"role":"user","content":"hi"}],"context_management":{"x":1}}`

func poolYAML(extra string, ups ...string) string {
	return "client_keys:\n  alice: tp-alice-0123456789\n  bob: tp-bob-0123456789ab\nadmin_key: tp-admin-0123456789\n" + extra +
		"upstreams:\n" + strings.Join(ups, "")
}

func claudeUp(url string) string {
	return fmt.Sprintf("  - {name: claude, url: %q, format: anthropic, token: sk-ant-claude}\n", url)
}

func grokUp(url string) string {
	return fmt.Sprintf("  - {name: grok, url: %q, format: openai, token: xai-grok, model: grok-4}\n", url+"/v1")
}

func TestFailsOverFromRateLimitedClaudeToGrok(t *testing.T) {
	claude := newFake(t, rateLimited)
	grok := newFake(t, grokStream)
	srv, p := newProxy(t, poolYAML("", claudeUp(claude.URL), grokUp(grok.URL)), "")

	res, body := call(t, "POST", srv.URL+"/v1/messages?beta=true", "tp-alice-0123456789", messagesBody,
		"Anthropic-Version", "2023-06-01", "Anthropic-Beta", "context-management-2025-06-27")
	if res.StatusCode != 200 || res.Header.Get("X-Tokenpool-Upstream") != "grok" {
		t.Fatalf("status %d upstream %q body %s", res.StatusCode, res.Header.Get("X-Tokenpool-Upstream"), body)
	}
	if !strings.Contains(body, "event: message_start") || !strings.Contains(body, `"text":"grok"`) ||
		!strings.Contains(body, "event: message_stop") {
		t.Errorf("not an Anthropic stream: %s", body)
	}

	c := claude.calls()[0]
	if c.path != "/v1/messages?beta=true" || c.header.Get("X-Api-Key") != "sk-ant-claude" ||
		c.header.Get("Anthropic-Beta") != "context-management-2025-06-27" {
		t.Errorf("claude got path %s headers %v", c.path, c.header)
	}
	if c.header.Get("Authorization") != "" || strings.Contains(fmt.Sprint(c.header), "tp-alice-0123456789") {
		t.Error("client key leaked upstream")
	}
	g := grok.calls()[0]
	if g.path != "/v1/chat/completions" || g.header.Get("Authorization") != "Bearer xai-grok" {
		t.Errorf("grok got path %s auth %q", g.path, g.header.Get("Authorization"))
	}
	if g.body["model"] != "grok-4" || g.body["stream"] != true || g.body["context_management"] != nil {
		t.Errorf("grok body = %s", g.raw)
	}

	// Claude is benched for its Retry-After: the next request goes straight to grok.
	st, _ := p.Get("claude")
	if st.State != "cooling" || !strings.Contains(st.CooldownReason, "rate limited") {
		t.Errorf("claude status = %+v", st)
	}
	call(t, "POST", srv.URL+"/v1/messages", "tp-bob-0123456789ab", messagesBody)
	if len(claude.calls()) != 1 || len(grok.calls()) != 2 {
		t.Errorf("calls: claude %d grok %d", len(claude.calls()), len(grok.calls()))
	}
}

// The pool from the README: two Claude keys, then grok as the fallback.
func TestClaudeKeysThenGrokFallback(t *testing.T) {
	primary := newFake(t, rateLimited)
	secondary := newFake(t, claudeMessage)
	grok := newFake(t, grokStream)
	srv, p := newProxy(t, poolYAML("", fmt.Sprintf(`
  - name: claude-primary
    url: %q
    format: anthropic
    token: sk-ant-primary
    priority: 1
  - name: claude-secondary
    url: %q
    format: anthropic
    token: sk-ant-secondary
    priority: 2
  - name: grok
    url: %q
    format: openai
    token: xai-grok
    fallback: true
    priority: 3
    models:
      "claude-opus-*": grok-4
    model: grok-4-fast
`, primary.URL, secondary.URL, grok.URL+"/v1")), "")

	nonStream := `{"model":"claude-sonnet-5","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	res, body := call(t, "POST", srv.URL+"/v1/messages", "tp-alice-0123456789", nonStream)
	if res.Header.Get("X-Tokenpool-Upstream") != "claude-secondary" || len(grok.calls()) != 0 {
		t.Fatalf("served by %q (%s), grok calls %d", res.Header.Get("X-Tokenpool-Upstream"), body, len(grok.calls()))
	}
	if got := secondary.calls()[0].header.Get("X-Api-Key"); got != "sk-ant-secondary" {
		t.Errorf("claude-secondary got key %q", got)
	}

	// Both Claude keys limited: grok takes over, with the model mapped.
	secondary.setReply(rateLimited)
	if _, err := p.Reset("claude-secondary"); err != nil {
		t.Fatal(err)
	}
	opus := `{"model":"claude-opus-5-5","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	res, body = call(t, "POST", srv.URL+"/v1/messages", "tp-alice-0123456789", opus)
	if res.StatusCode != 200 || res.Header.Get("X-Tokenpool-Upstream") != "grok" || !strings.Contains(body, "message_stop") {
		t.Fatalf("status %d upstream %q body %s", res.StatusCode, res.Header.Get("X-Tokenpool-Upstream"), body)
	}
	call(t, "POST", srv.URL+"/v1/messages", "tp-alice-0123456789", messagesBody)
	if got := []any{grok.calls()[0].body["model"], grok.calls()[1].body["model"]}; got[0] != "grok-4" || got[1] != "grok-4-fast" {
		t.Errorf("grok models = %v, want grok-4 for opus and grok-4-fast otherwise", got)
	}

	// Once a Claude key resets, traffic leaves the fallback.
	primary.setReply(claudeMessage)
	if _, err := p.Reset("claude-primary"); err != nil {
		t.Fatal(err)
	}
	res, _ = call(t, "POST", srv.URL+"/v1/messages", "tp-alice-0123456789", nonStream)
	if res.Header.Get("X-Tokenpool-Upstream") != "claude-primary" {
		t.Errorf("after reset served by %q", res.Header.Get("X-Tokenpool-Upstream"))
	}
}

func TestPassthroughKeepsTheRequestIntact(t *testing.T) {
	claude := newFake(t, claudeMessage)
	srv, _ := newProxy(t, poolYAML("", claudeUp(claude.URL)), "")
	body := `{"model":"claude-sonnet-5","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"betas_thing":[1,2]}`
	res, got := call(t, "POST", srv.URL+"/v1/messages", "tp-alice-0123456789", body)
	if res.StatusCode != 200 || !strings.Contains(got, "from claude") || res.Header.Get("Request-Id") != "req_123" {
		t.Fatalf("status %d body %s headers %v", res.StatusCode, got, res.Header)
	}
	if raw := claude.calls()[0].raw; raw != body {
		t.Errorf("body changed:\n%s\n%s", raw, body)
	}
}

func TestOpenAIClientReachesClaude(t *testing.T) {
	claude := newFake(t, claudeMessage)
	srv, _ := newProxy(t, poolYAML("", claudeUp(claude.URL)), "")
	res, body := call(t, "POST", srv.URL+"/v1/chat/completions", "",
		`{"model":"claude-sonnet-5","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`,
		"Authorization", "Bearer tp-alice-0123456789")
	if res.StatusCode != 200 {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Object != "chat.completion" ||
		out.Choices[0].Message.Content != "from claude" || out.Choices[0].FinishReason != "stop" {
		t.Errorf("response = %s", body)
	}
	c := claude.calls()[0]
	if c.path != "/v1/messages" || c.body["system"] != "sys" || c.body["max_tokens"] != 8192.0 ||
		c.header.Get("Anthropic-Version") != "2023-06-01" {
		t.Errorf("claude got %s %s", c.path, c.raw)
	}
}

func TestEveryUpstreamLimited(t *testing.T) {
	claude := newFake(t, rateLimited)
	grok := newFake(t, rateLimited)
	srv, _ := newProxy(t, poolYAML("", claudeUp(claude.URL), grokUp(grok.URL)), "")
	res, body := call(t, "POST", srv.URL+"/v1/messages", "tp-alice-0123456789", messagesBody)
	if res.StatusCode != 429 || res.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d retry-after %q", res.StatusCode, res.Header.Get("Retry-After"))
	}
	if !strings.Contains(body, `"rate_limit_error"`) || !strings.Contains(body, "claude: rate limited") ||
		!strings.Contains(body, "grok: rate limited") {
		t.Errorf("body = %s", body)
	}
	// While both cool down, nothing is sent upstream.
	res, _ = call(t, "POST", srv.URL+"/v1/chat/completions", "tp-alice-0123456789", `{"model":"x","messages":[]}`)
	if res.StatusCode != 429 || len(claude.calls())+len(grok.calls()) != 2 {
		t.Errorf("status %d, calls %d", res.StatusCode, len(claude.calls())+len(grok.calls()))
	}
}

func TestBadRequestIsNotRetried(t *testing.T) {
	claude := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(400)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: required"}}`)
	})
	grok := newFake(t, grokStream)
	srv, p := newProxy(t, poolYAML("", claudeUp(claude.URL), grokUp(grok.URL)), "")

	res, body := call(t, "POST", srv.URL+"/v1/messages", "tp-alice-0123456789", messagesBody)
	if res.StatusCode != 400 || !strings.Contains(body, "max_tokens: required") || len(grok.calls()) != 0 {
		t.Errorf("status %d body %s grok calls %d", res.StatusCode, body, len(grok.calls()))
	}
	// An OpenAI client gets the same rejection in OpenAI shape.
	res, body = call(t, "POST", srv.URL+"/v1/chat/completions", "tp-alice-0123456789", `{"model":"m","messages":[]}`)
	if res.StatusCode != 400 || !strings.Contains(body, `"error":{"code":null,"message":"claude: max_tokens: required"`) {
		t.Errorf("status %d body %s", res.StatusCode, body)
	}
	if st, _ := p.Get("claude"); st.State != "available" {
		t.Errorf("claude benched for a client error: %+v", st)
	}
}

func TestUnreachableUpstreamFailsOver(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	claude := newFake(t, claudeMessage)
	srv, p := newProxy(t, poolYAML("",
		fmt.Sprintf("  - {name: dead, url: %q, format: anthropic, token: t}\n", dead.URL),
		claudeUp(claude.URL)), "")
	res, body := call(t, "POST", srv.URL+"/v1/messages", "tp-alice-0123456789", `{"model":"m","max_tokens":5,"messages":[]}`)
	if res.StatusCode != 200 || !strings.Contains(body, "from claude") {
		t.Fatalf("status %d body %s", res.StatusCode, body)
	}
	if st, _ := p.Get("dead"); st.State != "cooling" || !strings.Contains(st.CooldownReason, "unreachable") {
		t.Errorf("dead = %+v", st)
	}
}

func TestClientAuth(t *testing.T) {
	claude := newFake(t, claudeMessage)
	srv, _ := newProxy(t, poolYAML("", claudeUp(claude.URL)), "")
	for _, key := range []string{"", "wrong", "tp-admin-0123456789"} {
		res, body := call(t, "POST", srv.URL+"/v1/messages", key, messagesBody)
		if res.StatusCode != 401 || !strings.Contains(body, "authentication_error") {
			t.Errorf("key %q: status %d body %s", key, res.StatusCode, body)
		}
	}
	if len(claude.calls()) != 0 {
		t.Error("unauthenticated request reached the upstream")
	}
}

func TestModelRouting(t *testing.T) {
	claude := newFake(t, claudeMessage)
	srv, _ := newProxy(t, poolYAML("",
		fmt.Sprintf("  - {name: claude, url: %q, format: anthropic, token: t, models: {\"claude-*\": \"\"}}\n", claude.URL)), "")
	res, body := call(t, "POST", srv.URL+"/v1/messages", "tp-alice-0123456789", `{"model":"gpt-4o","max_tokens":5,"messages":[]}`)
	if res.StatusCode != 404 || !strings.Contains(body, `no upstream in the pool serves model \"gpt-4o\"`) {
		t.Errorf("status %d body %s", res.StatusCode, body)
	}
	res, _ = call(t, "GET", srv.URL+"/v1/models", "tp-alice-0123456789", "")
	if res.StatusCode != 200 {
		t.Errorf("models status %d", res.StatusCode)
	}
}

func TestMaxTokensCap(t *testing.T) {
	grok := newFake(t, grokStream)
	srv, _ := newProxy(t, poolYAML("",
		fmt.Sprintf("  - {name: small, url: %q, format: openai, token: t, max_tokens: 2048}\n", grok.URL+"/v1")), "")
	call(t, "POST", srv.URL+"/v1/messages", "tp-alice-0123456789",
		`{"model":"m","max_tokens":64000,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if got := grok.calls()[0].body["max_tokens"]; got != 2048.0 {
		t.Errorf("max_tokens = %v", got)
	}
}

func TestCountTokens(t *testing.T) {
	grok := newFake(t, grokStream)
	srv, _ := newProxy(t, poolYAML("", grokUp(grok.URL)), "")
	res, body := call(t, "POST", srv.URL+"/v1/messages/count_tokens", "tp-alice-0123456789",
		`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hello there"}]}`)
	if res.StatusCode != 200 || res.Header.Get("X-Tokenpool-Estimated") != "true" ||
		!strings.Contains(body, `"input_tokens"`) || len(grok.calls()) != 0 {
		t.Errorf("status %d body %s", res.StatusCode, body)
	}

	claude := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		io.WriteString(w, `{"input_tokens":42}`)
	})
	srv, _ = newProxy(t, poolYAML("", grokUp(grok.URL), claudeUp(claude.URL)), "")
	_, body = call(t, "POST", srv.URL+"/v1/messages/count_tokens", "tp-alice-0123456789", `{"model":"m","messages":[]}`)
	if body != `{"input_tokens":42}` || claude.calls()[0].path != "/v1/messages/count_tokens" {
		t.Errorf("body %s", body)
	}
}

func TestAdminAddsUpstreamAtRuntime(t *testing.T) {
	grok := newFake(t, grokStream)
	file := filepath.Join(t.TempDir(), "pool.json")
	srv, _ := newProxy(t, poolYAML(""), file)

	res, _ := call(t, "GET", srv.URL+"/admin/upstreams", "tp-alice-0123456789", "")
	if res.StatusCode != 401 {
		t.Errorf("client key opened the admin API: %d", res.StatusCode)
	}
	res, body := call(t, "POST", srv.URL+"/v1/messages", "tp-alice-0123456789", messagesBody)
	if res.StatusCode != 404 {
		t.Errorf("empty pool: status %d body %s", res.StatusCode, body)
	}

	add := fmt.Sprintf(`{"name":"grok","url":%q,"format":"openai","token":"xai-a-long-secret-token-9999","model":"grok-4"}`, grok.URL+"/v1")
	res, body = call(t, "POST", srv.URL+"/admin/upstreams", "tp-admin-0123456789", add)
	if res.StatusCode != 201 || strings.Contains(body, "a-long-secret") {
		t.Fatalf("add: status %d body %s", res.StatusCode, body)
	}
	res, _ = call(t, "POST", srv.URL+"/admin/upstreams", "tp-admin-0123456789", add)
	if res.StatusCode != 409 {
		t.Errorf("duplicate add: %d", res.StatusCode)
	}
	res, _ = call(t, "POST", srv.URL+"/admin/upstreams", "tp-admin-0123456789", `{"name":"x","url":"ftp://x","format":"openai"}`)
	if res.StatusCode != 400 {
		t.Errorf("invalid add: %d", res.StatusCode)
	}

	res, body = call(t, "POST", srv.URL+"/v1/messages", "tp-alice-0123456789", messagesBody)
	if res.StatusCode != 200 || res.Header.Get("X-Tokenpool-Upstream") != "grok" {
		t.Errorf("after add: status %d body %s", res.StatusCode, body)
	}
	if got := grok.calls()[0].header.Get("Authorization"); got != "Bearer xai-a-long-secret-token-9999" {
		t.Errorf("grok auth = %q", got)
	}

	res, body = call(t, "POST", srv.URL+"/admin/upstreams/grok/disable", "tp-admin-0123456789", "")
	if res.StatusCode != 200 || !strings.Contains(body, `"state":"disabled"`) {
		t.Errorf("disable: %d %s", res.StatusCode, body)
	}
	res, _ = call(t, "POST", srv.URL+"/v1/messages", "tp-alice-0123456789", messagesBody)
	if res.StatusCode != 503 {
		t.Errorf("all disabled: status %d", res.StatusCode)
	}

	res, _ = call(t, "DELETE", srv.URL+"/admin/upstreams/grok", "tp-admin-0123456789", "")
	if res.StatusCode != 204 {
		t.Errorf("delete: %d", res.StatusCode)
	}
	res, _ = call(t, "GET", srv.URL+"/admin/upstreams/grok", "tp-admin-0123456789", "")
	if res.StatusCode != 404 {
		t.Errorf("get after delete: %d", res.StatusCode)
	}
}

func TestUpstreamURL(t *testing.T) {
	tests := []struct {
		url    string
		format config.Format
		k      kind
		want   string
	}{
		{"https://api.anthropic.com", config.Anthropic, kindMessages, "https://api.anthropic.com/v1/messages"},
		{"https://api.anthropic.com/v1/", config.Anthropic, kindCountTokens, "https://api.anthropic.com/v1/messages/count_tokens"},
		{"https://gw.corp/anthropic/v1/messages", config.Anthropic, kindMessages, "https://gw.corp/anthropic/v1/messages"},
		{"https://api.x.ai/v1", config.OpenAI, kindChat, "https://api.x.ai/v1/chat/completions"},
		{"https://api.x.ai", config.OpenAI, kindChat, "https://api.x.ai/v1/chat/completions"},
		{"http://gateway.example.com:8000/openai/v1/chat/completions", config.OpenAI, kindChat, "http://gateway.example.com:8000/openai/v1/chat/completions"},
	}
	for _, tt := range tests {
		got := upstreamURL(config.Upstream{URL: tt.url, Format: tt.format}, tt.k, "")
		if got != tt.want {
			t.Errorf("upstreamURL(%s) = %s, want %s", tt.url, got, tt.want)
		}
	}
}
