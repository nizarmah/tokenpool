package pool

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nizarmah/tokenpool/internal/config"
)

func up(name string, priority int) config.Upstream {
	return config.Upstream{Name: name, URL: "https://" + name + ".test", Format: config.OpenAI, Token: "tok-" + name, Priority: priority}
}

func names(c []Candidate) string {
	var out []string
	for _, x := range c {
		out = append(out, x.Name)
	}
	return strings.Join(out, ",")
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestFailoverOrderAndCooldown(t *testing.T) {
	p, err := New("failover", []config.Upstream{up("b", 1), up("a", 0), up("c", 1)}, "")
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Unix(1_000_000, 0)}
	p.SetClock(clk.now)

	sel := p.Pick("m")
	if got := names(sel.Candidates); got != "a,b,c" {
		t.Fatalf("order = %s, want priority then config order", got)
	}

	p.Fail(sel.Candidates[0].Upstream, 429, time.Minute, "rate limited")
	sel = p.Pick("m")
	if got := names(sel.Candidates); got != "b,c" {
		t.Fatalf("after failure = %s", got)
	}
	if len(sel.Unavailable) != 1 || !strings.Contains(sel.Unavailable[0], "a: rate limited") {
		t.Errorf("unavailable = %v", sel.Unavailable)
	}
	if sel.Wait != time.Minute {
		t.Errorf("wait = %s", sel.Wait)
	}

	clk.advance(61 * time.Second)
	if got := names(p.Pick("m").Candidates); got != "a,b,c" {
		t.Errorf("after cooldown = %s", got)
	}
}

func TestShorterFailureDoesNotShortenCooldown(t *testing.T) {
	p, _ := New("failover", []config.Upstream{up("a", 0)}, "")
	clk := &clock{t: time.Unix(1_000_000, 0)}
	p.SetClock(clk.now)
	u := p.Pick("m").Candidates[0].Upstream
	p.Fail(u, 429, time.Hour, "rate limited")
	p.Fail(u, 500, time.Second, "error")
	if st, _ := p.Get("a"); st.CooldownReason != "rate limited" || st.CooldownUntil.Sub(clk.t) != time.Hour {
		t.Errorf("cooldown = %v %q", st.CooldownUntil, st.CooldownReason)
	}
}

func TestFallbackGoesLastWhateverItsPriority(t *testing.T) {
	grok := up("grok", 0)
	grok.Fallback = true
	spare := up("spare", -5)
	spare.Fallback = true
	p, _ := New("failover", []config.Upstream{grok, up("claude-secondary", 2), spare, up("claude-primary", 1)}, "")
	clk := &clock{t: time.Unix(1_000_000, 0)}
	p.SetClock(clk.now)

	sel := p.Pick("claude-sonnet-5")
	if got := names(sel.Candidates); got != "claude-primary,claude-secondary,spare,grok" {
		t.Fatalf("order = %s, want primaries by priority, then fallbacks by priority", got)
	}
	for _, c := range sel.Candidates[:2] {
		p.Fail(c.Upstream, 429, time.Minute, "rate limited")
	}
	if got := names(p.Pick("claude-sonnet-5").Candidates); got != "spare,grok" {
		t.Errorf("with primaries benched = %s", got)
	}
	clk.advance(2 * time.Minute)
	if got := names(p.Pick("claude-sonnet-5").Candidates); got != "claude-primary,claude-secondary,spare,grok" {
		t.Errorf("after reset = %s, want primaries back in front", got)
	}
	if st, _ := p.Get("grok"); !st.Fallback {
		t.Error("status does not report fallback")
	}
}

func TestRoundRobinKeepsFallbackSeparate(t *testing.T) {
	fb := up("fb", 0)
	fb.Fallback = true
	p, _ := New("round_robin", []config.Upstream{up("a", 0), up("b", 0), fb}, "")
	for range 4 {
		if got := names(p.Pick("m").Candidates); !strings.HasSuffix(got, ",fb") {
			t.Fatalf("fallback rotated into the primaries: %s", got)
		}
	}
}

func TestRoundRobinWithinTier(t *testing.T) {
	p, _ := New("round_robin", []config.Upstream{up("a", 0), up("b", 0), up("c", 0), up("z", 9)}, "")
	seen := map[string]bool{}
	for range 3 {
		got := names(p.Pick("m").Candidates)
		if !strings.HasSuffix(got, ",z") {
			t.Fatalf("lower tier moved: %s", got)
		}
		seen[got[:1]] = true
	}
	if len(seen) != 3 {
		t.Errorf("first picks = %v, want each of a, b, c", seen)
	}
}

func TestModelEligibility(t *testing.T) {
	only := up("claude-only", 0)
	only.Models = map[string]string{"claude-*": ""}
	grok := up("grok", 1)
	grok.Model = "grok-4"
	p, _ := New("failover", []config.Upstream{only, grok}, "")

	sel := p.Pick("claude-sonnet-5")
	if got := names(sel.Candidates); got != "claude-only,grok" {
		t.Fatalf("claude model: %s", got)
	}
	if sel.Candidates[0].UpstreamModel != "claude-sonnet-5" || sel.Candidates[1].UpstreamModel != "grok-4" {
		t.Errorf("models = %q, %q", sel.Candidates[0].UpstreamModel, sel.Candidates[1].UpstreamModel)
	}
	if got := names(p.Pick("gpt-4o").Candidates); got != "grok" {
		t.Errorf("gpt model: %s", got)
	}
}

func TestDisabled(t *testing.T) {
	off := up("off", 0)
	off.Disabled = true
	p, _ := New("failover", []config.Upstream{off, up("on", 1)}, "")
	sel := p.Pick("m")
	if names(sel.Candidates) != "on" || len(sel.Unavailable) != 1 {
		t.Fatalf("got %s / %v", names(sel.Candidates), sel.Unavailable)
	}
	if _, err := p.SetDisabled("off", false); err != nil {
		t.Fatal(err)
	}
	if got := names(p.Pick("m").Candidates); got != "off,on" {
		t.Errorf("after enable: %s", got)
	}
}

func TestAdminChangesPersist(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pool.json")
	p, err := New("failover", []config.Upstream{up("static", 5)}, file)
	if err != nil {
		t.Fatal(err)
	}
	added := up("added", 0)
	added.Token = "sk-a-very-long-secret-token-1234"
	st, err := p.Add(added)
	if err != nil {
		t.Fatal(err)
	}
	if st.Token == added.Token || !strings.HasSuffix(st.Token, "1234") {
		t.Errorf("token not redacted: %q", st.Token)
	}
	if _, err := p.Add(added); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate add: %v", err)
	}
	if _, err := p.Add(config.Upstream{Name: "bad", URL: "nope", Format: "x"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid add: %v", err)
	}
	if err := p.Remove("static"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("remove static: %v", err)
	}
	if _, err := p.SetDisabled("added", true); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("pool file mode = %v, want 0600", info.Mode().Perm())
	}

	reloaded, err := New("failover", []config.Upstream{up("static", 5)}, file)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reloaded.Get("added")
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "api" || got.State != "disabled" {
		t.Errorf("reloaded = %+v", got)
	}

	// Update with an empty token keeps the old one.
	changed := up("added", 0)
	changed.Token = ""
	changed.Model = "new-model"
	if _, err := reloaded.Update("added", changed); err != nil {
		t.Fatal(err)
	}
	sel := reloaded.Pick("m")
	if sel.Candidates[0].Token != added.Token || sel.Candidates[0].UpstreamModel != "new-model" {
		t.Errorf("after update: token %q model %q", sel.Candidates[0].Token, sel.Candidates[0].UpstreamModel)
	}

	if err := reloaded.Remove("added"); err != nil {
		t.Fatal(err)
	}
	again, _ := New("failover", nil, file)
	if len(again.List()) != 0 {
		t.Errorf("removed upstream came back: %+v", again.List())
	}
}

func TestPoolFileClashWithConfig(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pool.json")
	p, _ := New("failover", nil, file)
	if _, err := p.Add(up("dup", 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := New("failover", []config.Upstream{up("dup", 0)}, file); err == nil {
		t.Error("want an error when the pool file and config share a name")
	}
}
