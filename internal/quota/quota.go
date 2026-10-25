// Package quota reads how much of its usage limits an upstream has left
// from the rate-limit headers it sends with every response.
package quota

import (
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Window is one usage limit an upstream reported.
type Window struct {
	// Name identifies the limit: "5h" and "7d" for a Claude subscription,
	// "codex.primary" and "codex.secondary" for a Codex login, "requests"
	// or "tokens" for API-key limits.
	Name string `json:"name"`
	// Window is the limit's length, such as "5h" or "7d", when known.
	Window string `json:"window,omitempty"`
	// UsedPercent and RemainingPercent add up to 100.
	UsedPercent      float64 `json:"used_percent"`
	RemainingPercent float64 `json:"remaining_percent"`
	// ResetsAt is when the window starts over, when known.
	ResetsAt *time.Time `json:"resets_at,omitempty"`
	// Status is the provider's own verdict, such as Anthropic's allowed,
	// allowed_warning or rejected.
	Status string `json:"status,omitempty"`
	// ModelSpecific marks a limit that applies to one model family only
	// (Claude's 7d_opus, a Codex model limit). It is shown but does not
	// count toward the upstream's overall remaining quota.
	ModelSpecific bool `json:"model_specific,omitempty"`
	// Rate marks an API key's requests or tokens counter. It refills
	// within a minute or so, so it says nothing about how much an account
	// has left: it is shown but does not count toward remaining quota.
	Rate bool `json:"rate,omitempty"`
}

var (
	unifiedUtil  = regexp.MustCompile(`^anthropic-ratelimit-unified-(.+)-utilization$`)
	unifiedState = regexp.MustCompile(`^anthropic-ratelimit-unified-(.+)-status$`)
	codexUsed    = regexp.MustCompile(`^x-(.+)-(primary|secondary)-used-percent$`)
	anthropicRem = regexp.MustCompile(`^anthropic-ratelimit-(.+)-remaining$`)
	openaiRem    = regexp.MustCompile(`^x-ratelimit-remaining(?:-(.+))?$`)
	windowName   = regexp.MustCompile(`^(\d+)([mhd])`)
)

// Parse reads every usage limit in h: Anthropic's unified subscription
// limits (anthropic-ratelimit-unified-5h-utilization and friends), Codex's
// (x-codex-primary-used-percent and friends), and the remaining/limit
// counters of API keys (anthropic-ratelimit-tokens-remaining,
// x-ratelimit-remaining-requests). Windows come back sorted by name.
func Parse(h http.Header, now time.Time) []Window {
	lower := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			lower[strings.ToLower(k)] = strings.TrimSpace(v[0])
		}
	}
	byName := map[string]Window{}
	for key := range lower {
		switch {
		case unifiedUtil.MatchString(key):
			name := unifiedUtil.FindStringSubmatch(key)[1]
			util, err := strconv.ParseFloat(lower[key], 64)
			if err != nil || math.IsNaN(util) {
				continue
			}
			w := newWindow(name, util*100)
			w.Window = windowLabel(name, 0)
			w.ResetsAt = resetTime(lower["anthropic-ratelimit-unified-"+name+"-reset"], now)
			w.Status = lower["anthropic-ratelimit-unified-"+name+"-status"]
			w.ModelSpecific = strings.Contains(name, "_")
			if w.Status == "rejected" {
				w = setUsed(w, 100)
			}
			byName[name] = w
		case codexUsed.MatchString(key):
			m := codexUsed.FindStringSubmatch(key)
			family, slot := m[1], m[2]
			used, err := strconv.ParseFloat(lower[key], 64)
			if err != nil || math.IsNaN(used) {
				continue
			}
			prefix := "x-" + family + "-" + slot
			minutes, _ := strconv.Atoi(lower[prefix+"-window-minutes"])
			if used == 0 && minutes == 0 && lower[prefix+"-reset-at"] == "" {
				continue // a slot the plan doesn't use: sent empty
			}
			w := newWindow(strings.ReplaceAll(family, "-", "_")+"."+slot, used)
			w.Window = windowLabel("", minutes)
			w.ResetsAt = resetTime(lower[prefix+"-reset-at"], now)
			w.ModelSpecific = family != "codex" && !strings.HasPrefix(family, "codex-secondary")
			byName[w.Name] = w
		}
	}
	for key := range lower {
		// A rejected unified limit with no utilization header still counts.
		if m := unifiedState.FindStringSubmatch(key); m != nil && lower[key] == "rejected" {
			if _, ok := byName[m[1]]; !ok && m[1] != "fallback" && m[1] != "overage" {
				w := newWindow(m[1], 100)
				w.Window = windowLabel(m[1], 0)
				w.Status = "rejected"
				w.ResetsAt = resetTime(lower["anthropic-ratelimit-unified-"+m[1]+"-reset"], now)
				w.ModelSpecific = strings.Contains(m[1], "_")
				byName[m[1]] = w
			}
		}
		if m := anthropicRem.FindStringSubmatch(key); m != nil && !strings.HasPrefix(m[1], "unified") {
			if w, ok := counter(m[1], lower[key], lower["anthropic-ratelimit-"+m[1]+"-limit"],
				lower["anthropic-ratelimit-"+m[1]+"-reset"], now); ok {
				byName[w.Name] = w
			}
		}
		if m := openaiRem.FindStringSubmatch(key); m != nil {
			name, suffix := m[1], ""
			if name != "" {
				suffix = "-" + name
			} else {
				name = "requests"
			}
			if w, ok := counter(name, lower[key], lower["x-ratelimit-limit"+suffix],
				lower["x-ratelimit-reset"+suffix], now); ok {
				byName[w.Name] = w
			}
		}
	}
	out := make([]Window, 0, len(byName))
	for _, w := range byName {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func newWindow(name string, used float64) Window {
	return setUsed(Window{Name: name}, used)
}

func setUsed(w Window, used float64) Window {
	used = math.Round(min(max(used, 0), 100)*10) / 10
	w.UsedPercent, w.RemainingPercent = used, math.Round((100-used)*10)/10
	return w
}

// counter turns a remaining/limit pair into a window.
func counter(name, remaining, limit, reset string, now time.Time) (Window, bool) {
	rem, err1 := strconv.ParseFloat(remaining, 64)
	lim, err2 := strconv.ParseFloat(limit, 64)
	if err1 != nil || err2 != nil || lim <= 0 {
		return Window{}, false
	}
	w := newWindow(name, 100*(1-rem/lim))
	w.ResetsAt = resetTime(reset, now)
	w.Rate = true
	return w, true
}

// windowLabel names a window's length: from a name such as "5h" or
// "7d_opus", or from a length in minutes.
func windowLabel(name string, minutes int) string {
	if m := windowName.FindStringSubmatch(name); m != nil {
		return m[1] + m[2]
	}
	switch {
	case minutes <= 0:
		return ""
	case minutes%(24*60) == 0:
		return strconv.Itoa(minutes/(24*60)) + "d"
	case minutes%60 == 0:
		return strconv.Itoa(minutes/60) + "h"
	}
	return strconv.Itoa(minutes) + "m"
}

func resetTime(v string, now time.Time) *time.Time {
	if v == "" {
		return nil
	}
	d, ok := ParseReset(v, now)
	if !ok {
		return nil
	}
	t := now.Add(d).Truncate(time.Second)
	return &t
}

// ParseReset reads a reset value as a duration from now. It understands
// RFC 3339 times, Unix timestamps (s or ms), Go durations ("6m0s", "20ms")
// and plain seconds.
func ParseReset(v string, now time.Time) (time.Duration, bool) {
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

// Current returns w as of now: a window whose reset time has passed has
// started over, so it is back to 0% used.
func Current(w Window, now time.Time) Window {
	if w.ResetsAt != nil && !now.Before(*w.ResetsAt) {
		w = setUsed(w, 0)
		w.ResetsAt, w.Status = nil, ""
	}
	return w
}

// Remaining is the share of quota left across ws: the smallest remaining
// percentage among the usage windows that apply to every model. Rate
// counters and model-specific windows do not count. binding names that
// window. known is false when ws holds no such window.
func Remaining(ws []Window, now time.Time) (pct float64, binding Window, known bool) {
	pct = 100
	for _, w := range ws {
		if w.ModelSpecific || w.Rate {
			continue
		}
		w = Current(w, now)
		if !known || w.RemainingPercent < pct {
			pct, binding, known = w.RemainingPercent, w, true
		}
	}
	return pct, binding, known
}

// Exhausted returns how long until every exhausted window in ws has reset:
// the latest reset among windows at 100% used or rejected. Model-specific
// windows (Claude's 7d_opus) are left out: they block one model, not the
// whole upstream. ok is false when no window is exhausted or none of them
// says when it resets.
func Exhausted(ws []Window, now time.Time) (time.Duration, bool) {
	var latest time.Time
	for _, w := range ws {
		if w.ModelSpecific || (w.RemainingPercent > 0 && w.Status != "rejected") || w.ResetsAt == nil {
			continue
		}
		if w.ResetsAt.After(latest) {
			latest = *w.ResetsAt
		}
	}
	if latest.IsZero() {
		return 0, false
	}
	return max(latest.Sub(now), 0), true
}

// Merge updates old with fresh readings: windows in fresh replace windows
// of the same name, others are kept.
func Merge(old, fresh []Window) []Window {
	if len(fresh) == 0 {
		return old
	}
	byName := make(map[string]Window, len(old)+len(fresh))
	for _, w := range old {
		byName[w.Name] = w
	}
	for _, w := range fresh {
		byName[w.Name] = w
	}
	out := make([]Window, 0, len(byName))
	for _, w := range byName {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
