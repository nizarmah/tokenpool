package proxy

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nizarmah/tokenpool/internal/config"
	"github.com/nizarmah/tokenpool/internal/translate"
)

type action int

const (
	// relay sends the upstream's answer to the client: the request itself
	// is at fault, so another upstream would not help.
	relay action = iota
	// skip tries the next upstream without benching this one.
	skip
	// bench tries the next upstream and benches this one for a cooldown.
	bench
)

type verdict struct {
	action   action
	cooldown time.Duration
	reason   string
	limit    bool // a rate, quota or capacity limit rather than an error
}

// classify decides what an upstream error response means for the pool.
func classify(status int, h http.Header, body []byte, cd config.Cooldowns, now time.Time) verdict {
	msg := translate.ErrorMessage(body)
	why := func(reason string) string {
		if msg == "" {
			return reason
		}
		if len(msg) > 160 {
			msg = msg[:160] + "…"
		}
		return fmt.Sprintf("%s: %s", reason, msg)
	}
	wait := func(def time.Duration) time.Duration {
		d, ok := ResetAfter(h, now)
		if !ok {
			d = def
		}
		return min(max(d, time.Second), cd.Max)
	}
	switch {
	case status == http.StatusTooManyRequests:
		return verdict{bench, wait(cd.RateLimit), why("rate limited"), true}
	case status == 529 || status == http.StatusServiceUnavailable:
		return verdict{bench, wait(cd.Overloaded), why("overloaded"), true}
	case status == http.StatusPaymentRequired:
		return verdict{bench, wait(cd.Quota), why("out of credits"), true}
	case (status == http.StatusBadRequest || status == http.StatusForbidden ||
		status == http.StatusUnprocessableEntity) && quotaMessage(string(body)):
		return verdict{bench, wait(cd.Quota), why("quota exhausted"), true}
	case status == http.StatusUnauthorized:
		return verdict{bench, cd.Auth, why("unauthorized (check the token)"), false}
	case status == http.StatusForbidden:
		return verdict{bench, cd.Auth, why("forbidden (check the token)"), false}
	case status == http.StatusNotFound:
		return verdict{skip, 0, why("not found (check the url and model)"), false}
	case status == http.StatusRequestTimeout || status >= 500:
		return verdict{bench, wait(cd.Error), why(fmt.Sprintf("upstream error %d", status)), false}
	}
	return verdict{action: relay}
}

var quotaWords = []string{
	"credit balance", "insufficient_quota", "insufficient quota", "quota",
	"billing", "spending limit", "usage limit", "out of credits",
	"exceeded your", "rate limit", "rate_limit",
}

func quotaMessage(body string) bool {
	body = strings.ToLower(body)
	for _, w := range quotaWords {
		if strings.Contains(body, w) {
			return true
		}
	}
	return false
}

// ResetAfter reads how long until an upstream accepts requests again from
// Retry-After or rate-limit reset headers (Anthropic, OpenAI and xAI shapes).
func ResetAfter(h http.Header, now time.Time) (time.Duration, bool) {
	if v := h.Get("Retry-After-Ms"); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms >= 0 {
			return time.Duration(ms * float64(time.Millisecond)), true
		}
	}
	if v := h.Get("Retry-After"); v != "" {
		if s, err := strconv.ParseFloat(v, 64); err == nil && s >= 0 {
			return time.Duration(s * float64(time.Second)), true
		}
		if t, err := http.ParseTime(v); err == nil {
			return max(t.Sub(now), 0), true
		}
	}
	// A *-reset header paired with a *-remaining of 0 names the exhausted
	// limit; take the latest such reset. Resets with no remaining counter
	// (e.g. Anthropic's unified limits) are a fallback: take the soonest.
	var exhausted, fallback time.Duration
	var haveExhausted, haveFallback bool
	for key, values := range h {
		lower := strings.ToLower(key)
		if !strings.Contains(lower, "ratelimit") || !strings.Contains(lower, "reset") || len(values) == 0 {
			continue
		}
		d, ok := parseReset(values[0], now)
		if !ok {
			continue
		}
		if remaining := h.Get(strings.Replace(lower, "reset", "remaining", 1)); remaining != "" {
			if n, err := strconv.ParseFloat(remaining, 64); err == nil && n <= 0 && d > exhausted {
				exhausted, haveExhausted = d, true
			}
			continue
		}
		if !haveFallback || d < fallback {
			fallback, haveFallback = d, true
		}
	}
	switch {
	case haveExhausted:
		return exhausted, true
	case haveFallback:
		return fallback, true
	}
	return 0, false
}

// parseReset understands RFC 3339 times, Unix timestamps (s or ms),
// Go durations ("6m0s", "20ms") and plain seconds.
func parseReset(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return max(t.Sub(now), 0), true
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		switch {
		case n > 1e12:
			return max(time.UnixMilli(int64(n)).Sub(now), 0), true
		case n > 1e9:
			return max(time.Unix(int64(n), 0).Sub(now), 0), true
		case n >= 0:
			return time.Duration(n * float64(time.Second)), true
		}
		return 0, false
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return d, true
	}
	return 0, false
}
