package pool

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/nizarmah/tokenpool/internal/config"
)

// mixed is a pool of two Claude upstreams and a Codex-style upstream that
// serves gpt-* natively and maps Claude models to gpt-5-codex.
func mixed() []config.Upstream {
	claudeA, claudeB := up("claude-a", 1), up("claude-b", 1)
	claudeA.Format, claudeB.Format = config.Anthropic, config.Anthropic
	claudeA.Models = map[string]string{"claude-*": ""}
	claudeB.Models = map[string]string{"claude-*": ""}
	codex := up("codex", 1)
	codex.Models = map[string]string{"gpt-*": "", "claude-*": "gpt-5-codex"}
	return []config.Upstream{codex, claudeA, claudeB}
}

func TestNativeUpstreamsFirstAmongEquals(t *testing.T) {
	p, _ := New("priority", mixed(), "")
	sel := p.PickFor(Request{Model: "claude-opus-5"})
	if got := names(sel.Candidates); got != "claude-a,claude-b,codex" {
		t.Fatalf("order = %s, want the native Claude upstreams before the mapped one", got)
	}
	if c := sel.Candidates[2]; c.Native || c.UpstreamModel != "gpt-5-codex" {
		t.Errorf("codex candidate = native %v model %s", c.Native, c.UpstreamModel)
	}
	if got := names(p.PickFor(Request{Model: "gpt-5-codex"}).Candidates); got != "codex" {
		t.Errorf("gpt request = %s", got)
	}
}

func TestPinnedRequestStaysNative(t *testing.T) {
	p, _ := New("priority", mixed(), "")
	sel := p.PickFor(Request{Model: "claude-opus-5", Pinned: true})
	if got := names(sel.Candidates); got != "claude-a,claude-b" {
		t.Fatalf("pinned = %s", got)
	}
	p.Fail(sel.Candidates[0].Upstream, 429, time.Hour, "limited")
	p.Fail(sel.Candidates[1].Upstream, 429, time.Hour, "limited")
	sel = p.PickFor(Request{Model: "claude-opus-5", Pinned: true})
	if len(sel.Candidates) != 0 || len(sel.Unavailable) != 2 || sel.Wait == 0 {
		t.Errorf("pinned with natives out = %+v", sel)
	}
	if got := names(p.PickFor(Request{Model: "claude-opus-5"}).Candidates); got != "codex" {
		t.Errorf("unpinned with natives out = %s, want the translation", got)
	}
}

func TestPriorityBeatsNativeness(t *testing.T) {
	p, _ := New("priority", mixed(), "")
	if _, err := p.SetPriority("codex", 0); err != nil {
		t.Fatal(err)
	}
	if got := names(p.PickFor(Request{Model: "claude-opus-5"}).Candidates); got != "codex,claude-a,claude-b" {
		t.Errorf("order = %s, want the priority-0 upstream first", got)
	}
}

func TestDefaultStrategy(t *testing.T) {
	p, err := Open(Options{Strategy: "default", Default: "codex", Upstreams: mixed()})
	if err != nil {
		t.Fatal(err)
	}
	sel := p.PickFor(Request{Model: "claude-opus-5"})
	if got := names(sel.Candidates); got != "codex,claude-a,claude-b" {
		t.Fatalf("order = %s, want the default first", got)
	}
	p.Fail(sel.Candidates[0].Upstream, 429, time.Minute, "limited")
	if got := names(p.PickFor(Request{Model: "claude-opus-5"}).Candidates); got != "claude-a,claude-b" {
		t.Errorf("default limited = %s", got)
	}
	// Pinned requests the default can't serve natively skip it.
	if got := names(p.PickFor(Request{Model: "claude-opus-5", Pinned: true}).Candidates); got != "claude-a,claude-b" {
		t.Errorf("pinned = %s", got)
	}
	if _, err := Open(Options{Default: "nope", Upstreams: mixed()}); err == nil {
		t.Error("want an error for a default that is not in the pool")
	}
}

func quotaHeader(remaining float64, reset time.Time) http.Header {
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", strconv.FormatFloat(100-remaining, 'f', -1, 64))
	h.Set("x-codex-primary-reset-at", strconv.FormatInt(reset.Unix(), 10))
	return h
}

func TestMostQuotaStrategy(t *testing.T) {
	p, _ := New("most-quota", mixed(), "")
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	p.SetClock(clk.now)
	ups := map[string]*Upstream{}
	for _, c := range p.PickFor(Request{Model: "claude-opus-5"}).Candidates {
		ups[c.Name] = c.Upstream
	}
	p.Observe(ups["claude-a"], quotaHeader(2, clk.t.Add(time.Hour)))
	p.Observe(ups["claude-b"], quotaHeader(25, clk.t.Add(time.Hour)))
	p.Observe(ups["codex"], quotaHeader(80, clk.t.Add(2*time.Hour)))
	if got := names(p.PickFor(Request{Model: "claude-opus-5"}).Candidates); got != "codex,claude-b,claude-a" {
		t.Fatalf("order = %s, want most quota first", got)
	}
	// Pinning still wins over quota.
	if got := names(p.PickFor(Request{Model: "claude-opus-5", Pinned: true}).Candidates); got != "claude-b,claude-a" {
		t.Errorf("pinned = %s", got)
	}
	// Once the Claude windows reset they have the most again.
	clk.advance(time.Hour + time.Second)
	if got := names(p.PickFor(Request{Model: "claude-opus-5"}).Candidates); got != "claude-a,claude-b,codex" {
		t.Errorf("after the Claude reset = %s", got)
	}
	// An upstream with no readings yet counts as full.
	p2, _ := New("most_quota", mixed(), "")
	if got := names(p2.PickFor(Request{Model: "claude-opus-5"}).Candidates); got != "claude-a,claude-b,codex" {
		t.Errorf("no readings = %s", got)
	}
	sum := p.Summarize()
	if sum.Strategy != MostQuota || sum.First != "claude-a" || len(sum.Upstreams) != 3 {
		t.Errorf("summary = %+v", sum)
	}
	if r := sum.Upstreams[2]; r.Name != "codex" || r.RemainingPercent == nil || *r.RemainingPercent != 80 ||
		r.Window != "codex.primary" || r.ResetsAt == nil {
		t.Errorf("codex row = %+v", r)
	}
	if r := sum.Upstreams[0]; r.RemainingPercent == nil || *r.RemainingPercent != 100 {
		t.Errorf("claude-a after its reset = %+v", r)
	}
}

func TestMostQuotaIgnoresAPIKeyRateCounters(t *testing.T) {
	// An API key listed first, with the better priority, and a subscription.
	p, _ := New("most_quota", []config.Upstream{up("key", 0), up("sub", 1)}, "")
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	p.SetClock(clk.now)
	ups := map[string]*Upstream{}
	for _, c := range p.PickFor(Request{Model: "gpt-5"}).Candidates {
		ups[c.Name] = c.Upstream
	}
	key := http.Header{}
	key.Set("x-ratelimit-limit-requests", "500")
	key.Set("x-ratelimit-remaining-requests", "499")
	key.Set("x-ratelimit-reset-requests", "120ms")
	p.Observe(ups["key"], key)
	p.Observe(ups["sub"], quotaHeader(60, clk.t.Add(time.Hour)))
	if got := names(p.PickFor(Request{Model: "gpt-5"}).Candidates); got != "sub,key" {
		t.Errorf("order = %s, want the subscription before the per-minute API key", got)
	}
	sum := p.Summarize()
	for _, r := range sum.Upstreams {
		if r.Name == "key" && r.RemainingPercent != nil {
			t.Errorf("key shows %v%% quota from a rate counter", *r.RemainingPercent)
		}
	}
	// Under priority the key still goes first.
	p2, _ := New("priority", []config.Upstream{up("key", 0), up("sub", 1)}, "")
	if got := names(p2.PickFor(Request{Model: "gpt-5"}).Candidates); got != "key,sub" {
		t.Errorf("priority order = %s", got)
	}
}

func TestFallbackStaysLastUnderEveryStrategy(t *testing.T) {
	ups := mixed()
	ups[0].Fallback = true // codex
	for _, strategy := range []string{"priority", "most_quota", "round_robin"} {
		p, _ := New(strategy, ups, "")
		if got := names(p.PickFor(Request{Model: "claude-opus-5"}).Candidates); got[len(got)-5:] != "codex" {
			t.Errorf("%s: order = %s", strategy, got)
		}
	}
	// The default strategy's default goes first even when it is a fallback.
	p, _ := Open(Options{Strategy: "default", Default: "codex", Upstreams: ups})
	if got := names(p.PickFor(Request{Model: "claude-opus-5"}).Candidates); got != "codex,claude-a,claude-b" {
		t.Errorf("default fallback = %s", got)
	}
}

func TestAdminRoutingChangesPersist(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pool.json")
	p, err := New("priority", mixed(), file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.SetPriority("claude-b", -1); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SetDisabled("claude-a", true); err != nil {
		t.Fatal(err)
	}
	if err := p.SetDefault("codex"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetStrategy("bogus"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bogus strategy: %v", err)
	}
	if err := p.SetDefault("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown default: %v", err)
	}
	if _, err := p.SetPriority("nope", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown priority: %v", err)
	}
	var saved poolFile
	data, _ := os.ReadFile(file)
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Strategy != Default || saved.Default == nil || *saved.Default != "codex" ||
		*saved.Overrides["claude-b"].Priority != -1 || !*saved.Overrides["claude-a"].Disabled {
		t.Errorf("pool file = %s", data)
	}

	// A restart with the same config keeps every change.
	reloaded, err := New("priority", mixed(), file)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(reloaded.PickFor(Request{Model: "claude-opus-5"}).Candidates); got != "codex,claude-b" {
		t.Errorf("after restart = %s", got)
	}
	if st, _ := reloaded.Get("claude-a"); st.State != "disabled" {
		t.Errorf("claude-a = %s", st.State)
	}
	if st, _ := reloaded.Get("claude-b"); st.Priority != -1 {
		t.Errorf("claude-b priority = %d", st.Priority)
	}
	if st, _ := reloaded.Get("codex"); !st.Default {
		t.Error("codex is not the default after restart")
	}

	// Clearing the default goes back to priority order.
	if err := reloaded.SetDefault(""); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.SetStrategy("most-quota"); err != nil {
		t.Fatal(err)
	}
	again, _ := New("priority", mixed(), file)
	if s := again.Summarize(); s.Strategy != MostQuota || s.DefaultUpstream != "" {
		t.Errorf("after clearing = %+v", s)
	}

	// An override for an upstream that left the config is ignored, and a
	// saved default that left the pool is dropped rather than fatal.
	if err := again.SetDefault("claude-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := New("priority", mixed()[:1], file); err != nil {
		t.Errorf("restart without claude-a: %v", err)
	}
}

func TestRemovingTheDefaultClearsIt(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pool.json")
	p, _ := New("priority", nil, file)
	if _, err := p.Add(up("api-1", 0)); err != nil {
		t.Fatal(err)
	}
	if err := p.SetDefault("api-1"); err != nil {
		t.Fatal(err)
	}
	if err := p.Remove("api-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := New("priority", nil, file); err != nil {
		t.Errorf("restart after removing the default: %v", err)
	}
}

func TestRuntimePriorityForAPIUpstream(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pool.json")
	p, _ := New("priority", []config.Upstream{up("static", 1)}, file)
	if _, err := p.Add(up("added", 5)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SetPriority("added", 0); err != nil {
		t.Fatal(err)
	}
	if got := names(p.Pick("m").Candidates); got != "added,static" {
		t.Errorf("order = %s", got)
	}
	reloaded, _ := New("priority", []config.Upstream{up("static", 1)}, file)
	if got := names(reloaded.Pick("m").Candidates); got != "added,static" {
		t.Errorf("after restart = %s", got)
	}
}
