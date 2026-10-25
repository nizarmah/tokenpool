package quota

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

var now = time.Unix(1_800_000_000, 0)

func header(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func byName(ws []Window) map[string]Window {
	m := map[string]Window{}
	for _, w := range ws {
		m[w.Name] = w
	}
	return m
}

func unix(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

func TestParseAnthropicUnified(t *testing.T) {
	h := header(
		"anthropic-ratelimit-unified-status", "allowed",
		"anthropic-ratelimit-unified-5h-utilization", "0.25",
		"anthropic-ratelimit-unified-5h-reset", unix(now.Add(2*time.Hour)),
		"anthropic-ratelimit-unified-5h-status", "allowed",
		"anthropic-ratelimit-unified-7d-utilization", "0.98",
		"anthropic-ratelimit-unified-7d-reset", unix(now.Add(3*24*time.Hour)),
		"anthropic-ratelimit-unified-7d-status", "allowed_warning",
		"anthropic-ratelimit-unified-7d_opus-utilization", "0.5",
		"anthropic-ratelimit-unified-reset", unix(now.Add(2*time.Hour)),
	)
	ws := byName(Parse(h, now))
	five, week, opus := ws["5h"], ws["7d"], ws["7d_opus"]
	if five.UsedPercent != 25 || five.RemainingPercent != 75 || five.Window != "5h" || five.Status != "allowed" {
		t.Errorf("5h = %+v", five)
	}
	if five.ResetsAt == nil || !five.ResetsAt.Equal(now.Add(2*time.Hour)) {
		t.Errorf("5h reset = %v", five.ResetsAt)
	}
	if week.RemainingPercent != 2 || week.Window != "7d" || week.Status != "allowed_warning" {
		t.Errorf("7d = %+v", week)
	}
	if !opus.ModelSpecific || opus.Window != "7d" {
		t.Errorf("7d_opus = %+v", opus)
	}
	pct, binding, ok := Remaining(Parse(h, now), now)
	if !ok || pct != 2 || binding.Name != "7d" {
		t.Errorf("remaining = %v %s %v, want 2%% on 7d", pct, binding.Name, ok)
	}
}

func TestParseAnthropicRejectedWithoutUtilization(t *testing.T) {
	h := header(
		"anthropic-ratelimit-unified-7d-status", "rejected",
		"anthropic-ratelimit-unified-7d-reset", unix(now.Add(time.Hour)),
		"anthropic-ratelimit-unified-overage-status", "rejected",
	)
	ws := byName(Parse(h, now))
	if w := ws["7d"]; w.RemainingPercent != 0 || w.Status != "rejected" {
		t.Errorf("7d = %+v", w)
	}
	if _, ok := ws["overage"]; ok {
		t.Error("overage is not a usage window")
	}
	if d, ok := Exhausted(Parse(h, now), now); !ok || d != time.Hour {
		t.Errorf("exhausted = %s %v", d, ok)
	}
}

func TestParseCodex(t *testing.T) {
	h := header(
		"x-codex-primary-used-percent", "12.5",
		"x-codex-primary-window-minutes", "300",
		"x-codex-primary-reset-at", unix(now.Add(time.Hour)),
		"x-codex-secondary-used-percent", "40",
		"x-codex-secondary-window-minutes", "10080",
		"x-codex-secondary-reset-at", unix(now.Add(5*24*time.Hour)),
		"x-codex-bengalfox-primary-used-percent", "90",
		// A slot the plan doesn't use comes empty.
		"x-codex-secondary-primary-used-percent", "0",
		"x-codex-secondary-primary-window-minutes", "0",
		"x-codex-secondary-primary-reset-at", "",
	)
	ws := byName(Parse(h, now))
	p, s, m := ws["codex.primary"], ws["codex.secondary"], ws["codex_bengalfox.primary"]
	if p.UsedPercent != 12.5 || p.RemainingPercent != 87.5 || p.Window != "5h" || p.ModelSpecific {
		t.Errorf("primary = %+v", p)
	}
	if s.RemainingPercent != 60 || s.Window != "7d" || s.ResetsAt == nil {
		t.Errorf("secondary = %+v", s)
	}
	if !m.ModelSpecific {
		t.Errorf("a model limit counts toward the whole login: %+v", m)
	}
	if w, ok := ws["codex_secondary.primary"]; ok {
		t.Errorf("empty slot became a window: %+v", w)
	}
	if pct, binding, _ := Remaining(Parse(h, now), now); pct != 60 || binding.Name != "codex.secondary" {
		t.Errorf("remaining = %v on %s", pct, binding.Name)
	}
}

func TestParseAPIKeyCounters(t *testing.T) {
	openai := header(
		"x-ratelimit-limit-requests", "100",
		"x-ratelimit-remaining-requests", "25",
		"x-ratelimit-reset-requests", "6m0s",
		"x-ratelimit-limit-tokens", "1000",
		"x-ratelimit-remaining-tokens", "900",
	)
	ws := byName(Parse(openai, now))
	if w := ws["requests"]; w.RemainingPercent != 25 || w.ResetsAt == nil || !w.ResetsAt.Equal(now.Add(6*time.Minute)) {
		t.Errorf("requests = %+v", w)
	}
	if w := ws["tokens"]; w.RemainingPercent != 90 || !w.Rate {
		t.Errorf("tokens = %+v", w)
	}
	// Per-minute counters refill too fast to say how much an account has
	// left, so they are not quota.
	if pct, _, ok := Remaining(Parse(openai, now), now); ok {
		t.Errorf("rate counters counted as quota: %v%%", pct)
	}

	anthropic := header(
		"anthropic-ratelimit-input-tokens-limit", "400000",
		"anthropic-ratelimit-input-tokens-remaining", "100000",
		"anthropic-ratelimit-input-tokens-reset", now.Add(30*time.Second).Format(time.RFC3339),
		"anthropic-ratelimit-requests-limit", "50",
		"anthropic-ratelimit-requests-remaining", "nope",
	)
	ws = byName(Parse(anthropic, now))
	if w := ws["input-tokens"]; w.RemainingPercent != 25 || w.ResetsAt == nil {
		t.Errorf("input-tokens = %+v", w)
	}
	if _, ok := ws["requests"]; ok {
		t.Error("an unparsable counter should be skipped")
	}
}

func TestModelLimitDoesNotExhaustTheUpstream(t *testing.T) {
	ws := Parse(header(
		"anthropic-ratelimit-unified-5h-status", "rejected",
		"anthropic-ratelimit-unified-5h-utilization", "1.0",
		"anthropic-ratelimit-unified-5h-reset", unix(now.Add(time.Hour)),
		"anthropic-ratelimit-unified-7d-utilization", "0.4",
		"anthropic-ratelimit-unified-7d-reset", unix(now.Add(72*time.Hour)),
		"anthropic-ratelimit-unified-7d_opus-utilization", "1.0",
		"anthropic-ratelimit-unified-7d_opus-status", "rejected",
		"anthropic-ratelimit-unified-7d_opus-reset", unix(now.Add(120*time.Hour)),
	), now)
	if d, ok := Exhausted(ws, now); !ok || d != time.Hour {
		t.Errorf("exhausted = %s %v, want 1h (the 5h window, not 7d_opus)", d, ok)
	}
	// Only the model limit is used up: nothing blocks the whole upstream.
	ws = Parse(header(
		"anthropic-ratelimit-unified-7d_opus-status", "rejected",
		"anthropic-ratelimit-unified-7d_opus-reset", unix(now.Add(120*time.Hour)),
	), now)
	if d, ok := Exhausted(ws, now); ok {
		t.Errorf("exhausted = %s, want none", d)
	}
}

func TestNoHeadersNoWindows(t *testing.T) {
	if ws := Parse(header("content-type", "application/json", "retry-after", "3"), now); len(ws) != 0 {
		t.Errorf("windows = %+v", ws)
	}
	if _, _, ok := Remaining(nil, now); ok {
		t.Error("no windows should be unknown")
	}
}

func TestWindowsResetOverTime(t *testing.T) {
	ws := Parse(header(
		"x-codex-primary-used-percent", "100",
		"x-codex-primary-reset-at", unix(now.Add(time.Hour)),
	), now)
	if pct, _, _ := Remaining(ws, now); pct != 0 {
		t.Fatalf("remaining = %v", pct)
	}
	if d, ok := Exhausted(ws, now); !ok || d != time.Hour {
		t.Errorf("exhausted = %s %v", d, ok)
	}
	later := now.Add(time.Hour + time.Second)
	if pct, _, _ := Remaining(ws, later); pct != 100 {
		t.Errorf("after the reset, remaining = %v", pct)
	}
}

func TestMergeKeepsOtherWindows(t *testing.T) {
	old := []Window{newWindow("5h", 10), newWindow("7d", 50)}
	merged := byName(Merge(old, []Window{newWindow("5h", 30)}))
	if merged["5h"].UsedPercent != 30 || merged["7d"].UsedPercent != 50 {
		t.Errorf("merged = %+v", merged)
	}
}

func TestParseReset(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want time.Duration
	}{
		{"30", 30 * time.Second},
		{"6m0s", 6 * time.Minute},
		{unix(now.Add(time.Minute)), time.Minute},
		{strconv.FormatInt(now.Add(2*time.Second).UnixMilli(), 10), 2 * time.Second},
		{now.Add(time.Hour).Format(time.RFC3339), time.Hour},
	} {
		if got, ok := ParseReset(tt.in, now); !ok || got != tt.want {
			t.Errorf("ParseReset(%q) = %s %v, want %s", tt.in, got, ok, tt.want)
		}
	}
	if _, ok := ParseReset("soon", now); ok {
		t.Error("garbage parsed")
	}
}
