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
	"testing"

	"github.com/nizarmah/tokenpool/internal/config"
	"github.com/nizarmah/tokenpool/internal/pool"
)

const bob = "tp-bob-0123456789ab"

// priorityYAML: alice is low, bob is high. The paid API key only serves
// high callers, and so does the Grok reserve.
const callerPriorities = "client_priorities:\n  alice: low\n  bob: high\n"

func subUp(name, url string, prio int) string {
	return fmt.Sprintf("  - {name: %s, url: %q, format: anthropic, token: sk-ant-oat01-%s, priority: %d}\n", name, url, name, prio)
}

func paidUp(url string, extra string) string {
	return fmt.Sprintf("  - {name: paid, url: %q, format: anthropic, token: sk-ant-api03-paid, priority: 0, "+
		"min_client_priority: high%s}\n", url, extra)
}

func TestLowCallerNeverReachesPaidUpstream(t *testing.T) {
	paid := newFake(t, claudeMessage)
	sub := newFake(t, claudeMessage)
	srv, _ := newProxy(t, poolYAML(callerPriorities, paidUp(paid.URL, ""), subUp("sub", sub.URL, 1)), "")

	// Paid has the best priority: bob (high) gets it, alice (low) skips it.
	res, body := call(t, "POST", srv.URL+"/v1/messages", bob, messagesBody)
	if res.StatusCode != 200 || res.Header.Get("X-Tokenpool-Upstream") != "paid" {
		t.Fatalf("high caller: %d %s from %q", res.StatusCode, body, res.Header.Get("X-Tokenpool-Upstream"))
	}
	res, body = call(t, "POST", srv.URL+"/v1/messages", alice, messagesBody)
	if res.StatusCode != 200 || res.Header.Get("X-Tokenpool-Upstream") != "sub" {
		t.Fatalf("low caller: %d %s from %q", res.StatusCode, body, res.Header.Get("X-Tokenpool-Upstream"))
	}
	if len(paid.calls()) != 1 {
		t.Errorf("paid upstream saw %d calls, want only bob's", len(paid.calls()))
	}
}

func TestLowCallerGetsNoFallbackUpstream(t *testing.T) {
	sub := newFake(t, rateLimited)
	grok := newFake(t, grokStream)
	reserve := fmt.Sprintf("  - {name: grok, url: %q, format: openai, token: xai-grok, model: grok-4, fallback: true, "+
		"min_client_priority: high}\n", grok.URL+"/v1")
	srv, _ := newProxy(t, poolYAML(callerPriorities, subUp("sub", sub.URL, 1), reserve), "")

	// Everything permitted is limited: alice gets a rate limit, not Grok,
	// and the message says why Grok was not tried.
	res, body := call(t, "POST", srv.URL+"/v1/messages", alice, messagesBody)
	if res.StatusCode != 429 || len(grok.calls()) != 0 {
		t.Fatalf("low caller: %d %s, grok calls %d", res.StatusCode, body, len(grok.calls()))
	}
	if !strings.Contains(body, "sub:") || !strings.Contains(body, "the caller's priority (low) excludes grok (needs high)") {
		t.Errorf("body = %s", body)
	}
	if res.Header.Get("X-Tokenpool-Refused") != "" || res.Header.Get("Retry-After") == "" {
		t.Errorf("a limit is not a refusal: headers %v", res.Header)
	}

	// bob is high: the same pool falls back to Grok for him.
	res, body = call(t, "POST", srv.URL+"/v1/messages", bob, messagesBody)
	if res.StatusCode != 200 || res.Header.Get("X-Tokenpool-Upstream") != "grok" {
		t.Errorf("high caller: %d %s from %q", res.StatusCode, body, res.Header.Get("X-Tokenpool-Upstream"))
	}
}

func TestRefusedByPolicyErrorShapes(t *testing.T) {
	paid := newFake(t, claudeMessage)
	srv, _ := newProxy(t, poolYAML(callerPriorities, paidUp(paid.URL, "")), "")

	// Anthropic shape on /v1/messages.
	res, body := call(t, "POST", srv.URL+"/v1/messages", alice, messagesBody)
	var anth struct {
		Type  string `json:"type"`
		Error struct{ Type, Message string }
	}
	if err := json.Unmarshal([]byte(body), &anth); err != nil || res.StatusCode != 403 ||
		anth.Type != "error" || anth.Error.Type != "permission_error" ||
		!strings.Contains(anth.Error.Message, "refused by policy") ||
		!strings.Contains(anth.Error.Message, "priority (low) excludes paid (needs high)") {
		t.Errorf("anthropic: %d %s", res.StatusCode, body)
	}
	if res.Header.Get("X-Tokenpool-Refused") != "caller-priority" {
		t.Errorf("missing refusal header: %v", res.Header)
	}

	// OpenAI shape on /v1/responses.
	res, body = call(t, "POST", srv.URL+"/v1/responses", alice, `{"model":"claude-sonnet-5","input":"hi"}`)
	var oai struct {
		Type  string `json:"type"`
		Error struct{ Type, Message string }
	}
	if err := json.Unmarshal([]byte(body), &oai); err != nil || res.StatusCode != 403 ||
		oai.Type != "" || oai.Error.Type != "permission_error" ||
		!strings.Contains(oai.Error.Message, "priority (low) excludes paid") {
		t.Errorf("openai: %d %s", res.StatusCode, body)
	}

	// Not a missing model: a model nobody serves is still a 404.
	srvEmpty, _ := newProxy(t, poolYAML(callerPriorities), "")
	if res, _ = call(t, "POST", srvEmpty.URL+"/v1/messages", alice, messagesBody); res.StatusCode != 404 {
		t.Errorf("empty pool: %d", res.StatusCode)
	}
	if len(paid.calls()) != 0 {
		t.Errorf("paid upstream was called %d times", len(paid.calls()))
	}

	// A caller with no entry is normal, which a high-only upstream refuses too.
	cfg := "client_priorities:\n  alice: normal\n"
	srv2, _ := newProxy(t, poolYAML(cfg, paidUp(paid.URL, "")), "")
	if res, body = call(t, "POST", srv2.URL+"/v1/messages", bob, messagesBody); res.StatusCode != 403 ||
		!strings.Contains(body, "priority (normal) excludes") {
		t.Errorf("unranked caller: %d %s", res.StatusCode, body)
	}
}

func TestResponsesAndCountTokensRespectCallerPriority(t *testing.T) {
	paid := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) { io.WriteString(w, `{"input_tokens":42}`) })
	grok := newFake(t, grokStream)
	srv, _ := newProxy(t, poolYAML(callerPriorities, paidUp(paid.URL, ""), grokUp(grok.URL)), "")

	// count_tokens: alice's count is estimated, never sent to the paid key.
	body := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hello there"}]}`
	res, out := call(t, "POST", srv.URL+"/v1/messages/count_tokens", alice, body)
	if res.StatusCode != 200 || res.Header.Get("X-Tokenpool-Estimated") != "true" || len(paid.calls()) != 0 {
		t.Errorf("low count: %d %s, paid calls %d", res.StatusCode, out, len(paid.calls()))
	}
	res, out = call(t, "POST", srv.URL+"/v1/messages/count_tokens", bob, body)
	if res.StatusCode != 200 || out != `{"input_tokens":42}` {
		t.Errorf("high count: %d %s", res.StatusCode, out)
	}

	// Responses: alice gets Grok, bob (paid has priority 0) the paid Claude key.
	req := `{"model":"claude-sonnet-5","input":"hi","max_output_tokens":50}`
	res, out = call(t, "POST", srv.URL+"/v1/responses", alice, req)
	if res.StatusCode != 200 || res.Header.Get("X-Tokenpool-Upstream") != "grok" {
		t.Errorf("low responses: %d %s from %q", res.StatusCode, out, res.Header.Get("X-Tokenpool-Upstream"))
	}
	paid.setReply(claudeMessage)
	res, out = call(t, "POST", srv.URL+"/v1/responses", bob, req)
	if res.StatusCode != 200 || res.Header.Get("X-Tokenpool-Upstream") != "paid" {
		t.Errorf("high responses: %d %s from %q", res.StatusCode, out, res.Header.Get("X-Tokenpool-Upstream"))
	}
}

func TestServedLogNamesCallerPriority(t *testing.T) {
	paid := newFake(t, claudeMessage)
	cfg, err := config.Parse([]byte(poolYAML(callerPriorities, paidUp(paid.URL, ""))))
	if err != nil {
		t.Fatal(err)
	}
	p, err := pool.Open(pool.Options{Strategy: cfg.Strategy, Upstreams: cfg.Upstreams})
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	srv := httptest.NewServer(New(cfg, p, slog.New(slog.NewTextHandler(&logs, nil))).Handler())
	defer srv.Close()
	call(t, "POST", srv.URL+"/v1/messages", bob, messagesBody)
	call(t, "POST", srv.URL+"/v1/messages", alice, messagesBody)
	out := logs.String()
	if !strings.Contains(out, "msg=served client=bob priority=high") ||
		!strings.Contains(out, "msg=\"refused by caller priority\" client=alice priority=low") {
		t.Errorf("logs:\n%s", out)
	}
}

func TestAdminMinClientPriorityRoundTrip(t *testing.T) {
	paid := newFake(t, claudeMessage)
	sub := newFake(t, claudeMessage)
	file := filepath.Join(t.TempDir(), "pool.json")
	yaml := poolYAML(callerPriorities, paidUp(paid.URL, ""), subUp("sub", sub.URL, 1))
	srv, _ := newProxy(t, yaml, file)
	get := func(srvURL, path string) string {
		t.Helper()
		res, out := call(t, "GET", srvURL+path, admin, "")
		if res.StatusCode != 200 {
			t.Fatalf("GET %s: %d %s", path, res.StatusCode, out)
		}
		return out
	}
	servedBy := func(key string) string {
		res, body := call(t, "POST", srv.URL+"/v1/messages", key, messagesBody)
		if res.StatusCode != 200 {
			t.Fatalf("%d %s", res.StatusCode, body)
		}
		return res.Header.Get("X-Tokenpool-Upstream")
	}

	// GET shows the field (the default is low) and /admin/status lists callers.
	if out := get(srv.URL, "/admin/upstreams/paid"); !strings.Contains(out, `"min_client_priority":"high"`) {
		t.Errorf("paid: %s", out)
	}
	if out := get(srv.URL, "/admin/upstreams/sub"); !strings.Contains(out, `"min_client_priority":"low"`) {
		t.Errorf("sub: %s", out)
	}
	status := get(srv.URL, "/admin/status")
	if !strings.Contains(status, `"callers":[{"name":"alice","priority":"low"},{"name":"bob","priority":"high"}]`) ||
		!strings.Contains(status, `"min_client_priority":"high"`) || strings.Contains(status, "tp-alice") {
		t.Errorf("status: %s", status)
	}

	// Lower the bar on a config upstream: alice reaches paid. Raise it on sub: she doesn't reach sub.
	res, out := call(t, "POST", srv.URL+"/admin/upstreams/paid/min_client_priority", admin, `{"min_client_priority":"Low"}`)
	if res.StatusCode != 200 || !strings.Contains(out, `"min_client_priority":"low"`) {
		t.Fatalf("lower: %d %s", res.StatusCode, out)
	}
	if got := servedBy(alice); got != "paid" {
		t.Errorf("after lowering, alice served by %q", got)
	}
	call(t, "POST", srv.URL+"/admin/upstreams/paid/min_client_priority", admin, `{"min_client_priority":"high"}`)
	if got := servedBy(alice); got != "sub" {
		t.Errorf("after raising, alice served by %q", got)
	}
	for _, bad := range []string{`{}`, `{"min_client_priority":"urgent"}`, `{"priority":"high"}`} {
		if res, _ = call(t, "POST", srv.URL+"/admin/upstreams/paid/min_client_priority", admin, bad); res.StatusCode != 400 {
			t.Errorf("%s: %d", bad, res.StatusCode)
		}
	}
	if res, _ = call(t, "POST", srv.URL+"/admin/upstreams/nope/min_client_priority", admin, `{"min_client_priority":"high"}`); res.StatusCode != 404 {
		t.Errorf("unknown upstream: %d", res.StatusCode)
	}

	// An upstream added through the API takes the field on POST and on PUT.
	extra := newFake(t, claudeMessage)
	add := fmt.Sprintf(`{"name":"extra","url":%q,"format":"anthropic","token":"sk-ant-api03-extra","priority":-1,"min_client_priority":"normal"}`, extra.URL)
	res, out = call(t, "POST", srv.URL+"/admin/upstreams", admin, add)
	if res.StatusCode != 201 || !strings.Contains(out, `"min_client_priority":"normal"`) {
		t.Fatalf("add: %d %s", res.StatusCode, out)
	}
	if got := servedBy(alice); got != "sub" { // alice is low: extra (normal) is closed to her
		t.Errorf("alice served by %q, want sub", got)
	}
	if got := servedBy(bob); got != "extra" {
		t.Errorf("bob served by %q, want extra", got)
	}
	put := fmt.Sprintf(`{"url":%q,"format":"anthropic","priority":-1,"min_client_priority":"high"}`, extra.URL)
	if res, out = call(t, "PUT", srv.URL+"/admin/upstreams/extra", admin, put); res.StatusCode != 200 ||
		!strings.Contains(out, `"min_client_priority":"high"`) {
		t.Fatalf("put: %d %s", res.StatusCode, out)
	}
	if res, out = call(t, "PUT", srv.URL+"/admin/upstreams/extra", admin,
		fmt.Sprintf(`{"url":%q,"format":"anthropic","min_client_priority":"urgent"}`, extra.URL)); res.StatusCode != 400 {
		t.Errorf("bad put: %d %s", res.StatusCode, out)
	}

	// Everything survives a restart: the override on the config upstream
	// and the field on the API upstream.
	srv2, _ := newProxy(t, yaml, file)
	if out := get(srv2.URL, "/admin/upstreams/extra"); !strings.Contains(out, `"min_client_priority":"high"`) {
		t.Errorf("extra after restart: %s", out)
	}
	call(t, "POST", srv.URL+"/admin/upstreams/sub/min_client_priority", admin, `{"min_client_priority":"high"}`)
	srv3, _ := newProxy(t, yaml, file)
	if out := get(srv3.URL, "/admin/upstreams/sub"); !strings.Contains(out, `"min_client_priority":"high"`) {
		t.Errorf("sub after restart: %s", out)
	}
	if res, body := call(t, "POST", srv3.URL+"/v1/messages", alice, messagesBody); res.StatusCode != 403 {
		t.Errorf("alice after restart: %d %s", res.StatusCode, body)
	}
}
