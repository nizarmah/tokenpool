package proxy

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/nizarmah/tokenpool/internal/config"
)

var now = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func header(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestResetAfter(t *testing.T) {
	tests := []struct {
		name string
		h    http.Header
		want time.Duration
		ok   bool
	}{
		{"retry-after seconds", header("Retry-After", "30"), 30 * time.Second, true},
		{"retry-after date", header("Retry-After", now.Add(time.Minute).Format(http.TimeFormat)), time.Minute, true},
		{"retry-after-ms wins", header("Retry-After-Ms", "1500", "Retry-After", "9"), 1500 * time.Millisecond, true},
		{"anthropic exhausted limit", header(
			"Anthropic-Ratelimit-Requests-Remaining", "10",
			"Anthropic-Ratelimit-Requests-Reset", now.Add(time.Second).Format(time.RFC3339),
			"Anthropic-Ratelimit-Tokens-Remaining", "0",
			"Anthropic-Ratelimit-Tokens-Reset", now.Add(40*time.Second).Format(time.RFC3339),
		), 40 * time.Second, true},
		{"openai durations", header(
			"X-Ratelimit-Remaining-Requests", "0", "X-Ratelimit-Reset-Requests", "6m0s",
			"X-Ratelimit-Remaining-Tokens", "500", "X-Ratelimit-Reset-Tokens", "20ms",
		), 6 * time.Minute, true},
		{"unix reset without remaining", header(
			"Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(now.Add(3*time.Hour).Unix(), 10),
		), 3 * time.Hour, true},
		{"nothing", header("Content-Type", "application/json"), 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ResetAfter(tt.h, now)
			if got != tt.want || ok != tt.ok {
				t.Errorf("got %s, %v; want %s, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	cd := config.Cooldowns{RateLimit: time.Minute, Overloaded: 15 * time.Second, Error: 30 * time.Second,
		Auth: 10 * time.Minute, Quota: time.Hour, Max: 2 * time.Hour}
	tests := []struct {
		status int
		h      http.Header
		body   string
		action action
		cool   time.Duration
		limit  bool
	}{
		{429, header("Retry-After", "5"), `{"error":{"message":"slow"}}`, bench, 5 * time.Second, true},
		{429, header(), ``, bench, time.Minute, true},
		{429, header("Retry-After", "999999"), ``, bench, 2 * time.Hour, true}, // capped at max
		{529, header(), `{"type":"error","error":{"type":"overloaded_error"}}`, bench, 15 * time.Second, true},
		{503, header(), ``, bench, 15 * time.Second, true},
		{402, header(), ``, bench, time.Hour, true},
		{400, header(), `{"error":{"message":"Your credit balance is too low to access the Anthropic API."}}`, bench, time.Hour, true},
		{403, header(), `{"error":{"message":"You exceeded your current quota"}}`, bench, time.Hour, true},
		{401, header(), `{"error":{"message":"invalid x-api-key"}}`, bench, 10 * time.Minute, false},
		{403, header(), `{"error":{"message":"forbidden"}}`, bench, 10 * time.Minute, false},
		{500, header(), ``, bench, 30 * time.Second, false},
		{404, header(), `{"error":{"message":"model not found"}}`, skip, 0, false},
		{400, header(), `{"error":{"message":"messages: field required"}}`, relay, 0, false},
		{413, header(), ``, relay, 0, false},
	}
	for _, tt := range tests {
		v := classify(tt.status, tt.h, []byte(tt.body), cd, now)
		if v.action != tt.action || v.cooldown != tt.cool || v.limit != tt.limit {
			t.Errorf("classify(%d, %s) = %+v; want action %d cooldown %s limit %v",
				tt.status, tt.body, v, tt.action, tt.cool, tt.limit)
		}
	}
}
