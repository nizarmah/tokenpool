// Package pool tracks the upstreams tokenpool can send a request to, which
// of them are cooling down after hitting a limit, and which to try next.
package pool

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/nizarmah/tokenpool/internal/config"
)

var (
	ErrNotFound = errors.New("no upstream with that name")
	ErrExists   = errors.New("an upstream with that name already exists")
	ErrReadOnly = errors.New("upstream is defined in the config file; edit it there")
	ErrInvalid  = errors.New("invalid upstream")
)

// Upstream is a pool member. Its config never changes after creation;
// updates replace the whole Upstream.
type Upstream struct {
	config.Upstream // resolved: env references expanded

	raw     config.Upstream // as written, for the pool file
	static  bool            // from the config file, not the admin API
	seq     int
	fileTok *fileToken // set when the token comes from token_file

	// Guarded by Pool.mu.
	disabled      bool
	cooldownUntil time.Time
	reason        string
	lastStatus    int
	requests      int64
	failures      int64
	lastUsed      time.Time
}

// Credential is the secret to send upstream.
type Credential struct {
	Token string
	// GrokSession is true when Token was read from a Grok ~/.grok/auth.json,
	// which the CLI chat proxy accepts as a bearer session rather than an API key.
	GrokSession bool
}

// Credential returns the token to send upstream, reading token_file when
// the upstream has one.
func (u *Upstream) Credential() (Credential, error) {
	if u.fileTok == nil {
		return Credential{Token: u.Token}, nil
	}
	token, grok, err := u.fileTok.get()
	if err != nil {
		return Credential{}, err
	}
	return Credential{Token: token, GrokSession: grok}, nil
}

// AuthToken returns the token to send upstream, reading token_file when
// the upstream has one.
func (u *Upstream) AuthToken() (string, error) {
	cred, err := u.Credential()
	return cred.Token, err
}

// Candidate is an upstream chosen for a request, with the model to send it.
type Candidate struct {
	*Upstream
	UpstreamModel string
}

// Selection is the result of Pick.
type Selection struct {
	// Candidates are the upstreams to try, in order.
	Candidates []Candidate
	// Unavailable describes eligible upstreams that are disabled or cooling.
	Unavailable []string
	// Wait is how long until the soonest cooling upstream is back.
	Wait time.Duration
}

// Pool is safe for concurrent use.
type Pool struct {
	mu       sync.Mutex
	ups      []*Upstream
	seq      int
	strategy string
	rr       uint64
	file     string
	now      func() time.Time
}

// New builds a pool from the config file's upstreams plus any saved in file.
func New(strategy string, static []config.Upstream, file string) (*Pool, error) {
	p := &Pool{strategy: strategy, file: file, now: time.Now}
	for _, u := range static {
		// Fail at startup on a token_file that can't be read or parsed.
		if _, err := p.insert(u, u, true).AuthToken(); err != nil {
			return nil, fmt.Errorf("upstream %q: %w", u.Name, err)
		}
	}
	if file == "" {
		return p, nil
	}
	saved, err := readPoolFile(file)
	if err != nil {
		return nil, err
	}
	for _, raw := range saved {
		resolved, err := raw.Resolve()
		if err != nil {
			return nil, fmt.Errorf("%s: upstream %q: %w", file, raw.Name, err)
		}
		if err := resolved.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		if raw.TokenFile != "" {
			return nil, fmt.Errorf("%s: upstream %q: token_file can only be set in the config file", file, raw.Name)
		}
		if p.find(raw.Name) != nil {
			return nil, fmt.Errorf("%s: upstream %q is also in the config file", file, raw.Name)
		}
		p.insert(raw, resolved, false)
	}
	return p, nil
}

// SetClock replaces time.Now, for tests.
func (p *Pool) SetClock(now func() time.Time) { p.now = now }

func (p *Pool) insert(raw, resolved config.Upstream, static bool) *Upstream {
	p.seq++
	u := &Upstream{Upstream: resolved, raw: raw, static: static, seq: p.seq, disabled: resolved.Disabled}
	if resolved.TokenFile != "" {
		u.fileTok = &fileToken{path: resolved.TokenFile, field: resolved.TokenField}
	}
	p.add(u)
	return u
}

// add puts u in the pool, keeping it ordered: primaries before fallbacks,
// then by priority, then by insertion.
func (p *Pool) add(u *Upstream) {
	p.ups = append(p.ups, u)
	sort.SliceStable(p.ups, func(i, j int) bool {
		a, b := p.ups[i], p.ups[j]
		if a.Fallback != b.Fallback {
			return !a.Fallback
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return a.seq < b.seq
	})
}

func (p *Pool) find(name string) *Upstream {
	for _, u := range p.ups {
		if u.Name == name {
			return u
		}
	}
	return nil
}

// Pick returns the upstreams that can serve model right now, best first.
func (p *Pool) Pick(model string) Selection {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var sel Selection
	var soonest time.Time
	for _, u := range p.ups {
		upstreamModel, ok := u.MapModel(model)
		if !ok {
			continue
		}
		switch {
		case u.disabled:
			sel.Unavailable = append(sel.Unavailable, u.Name+": disabled")
		case now.Before(u.cooldownUntil):
			left := u.cooldownUntil.Sub(now)
			sel.Unavailable = append(sel.Unavailable,
				fmt.Sprintf("%s: %s (back in %s)", u.Name, u.reason, left.Round(time.Second)))
			if soonest.IsZero() || u.cooldownUntil.Before(soonest) {
				soonest = u.cooldownUntil
			}
		default:
			sel.Candidates = append(sel.Candidates, Candidate{Upstream: u, UpstreamModel: upstreamModel})
		}
	}
	if !soonest.IsZero() {
		sel.Wait = soonest.Sub(now)
	}
	if p.strategy == "round_robin" {
		p.rr++
		rotateTiers(sel.Candidates, p.rr)
	}
	return sel
}

// rotateTiers rotates each run of candidates sharing a priority (and
// fallback role) by n, so load spreads within a tier while later tiers
// stay in reserve.
func rotateTiers(c []Candidate, n uint64) {
	for start := 0; start < len(c); {
		end := start + 1
		for end < len(c) && c[end].Priority == c[start].Priority && c[end].Fallback == c[start].Fallback {
			end++
		}
		tier := c[start:end]
		if k := int(n % uint64(len(tier))); k > 0 {
			rotated := append(append([]Candidate{}, tier[k:]...), tier[:k]...)
			copy(tier, rotated)
		}
		start = end
	}
}

// Success records a request the upstream answered without failing over.
func (p *Pool) Success(u *Upstream, status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	u.requests++
	u.lastStatus = status
	u.lastUsed = p.now()
}

// Fail records a failure and benches the upstream for cooldown.
func (p *Pool) Fail(u *Upstream, status int, cooldown time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	u.requests++
	u.failures++
	u.lastStatus = status
	u.lastUsed = now
	if until := now.Add(cooldown); until.After(u.cooldownUntil) {
		u.cooldownUntil = until
		u.reason = reason
	}
}

// Record counts a request without benching the upstream, for answers that
// were the request's fault rather than the upstream's.
func (p *Pool) Record(u *Upstream, status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	u.requests++
	u.lastStatus = status
	u.lastUsed = p.now()
}

// Wait returns how long until any upstream that serves model is back, or 0.
func (p *Pool) Wait(model string) time.Duration {
	sel := p.Pick(model)
	if len(sel.Candidates) > 0 {
		return 0
	}
	return sel.Wait
}

// Status is the admin view of an upstream. Tokens are redacted.
type Status struct {
	Name           string            `json:"name"`
	URL            string            `json:"url"`
	Format         config.Format     `json:"format"`
	Auth           string            `json:"auth"`
	Token          string            `json:"token,omitempty"`
	TokenFile      string            `json:"token_file,omitempty"`
	Model          string            `json:"model,omitempty"`
	Models         map[string]string `json:"models,omitempty"`
	MaxTokens      int               `json:"max_tokens,omitempty"`
	Headers        []string          `json:"headers,omitempty"`
	Priority       int               `json:"priority"`
	Fallback       bool              `json:"fallback"`
	Source         string            `json:"source"`
	State          string            `json:"state"`
	CooldownUntil  *time.Time        `json:"cooldown_until,omitempty"`
	CooldownReason string            `json:"cooldown_reason,omitempty"`
	LastStatus     int               `json:"last_status,omitempty"`
	Requests       int64             `json:"requests"`
	Failures       int64             `json:"failures"`
	LastUsed       *time.Time        `json:"last_used,omitempty"`
}

// List returns every upstream in pool order.
func (p *Pool) List() []Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Status, 0, len(p.ups))
	for _, u := range p.ups {
		out = append(out, p.status(u))
	}
	return out
}

// Get returns one upstream's status.
func (p *Pool) Get(name string) (Status, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	u := p.find(name)
	if u == nil {
		return Status{}, ErrNotFound
	}
	return p.status(u), nil
}

// statusAuth reports the auth style, including a Grok login file whose
// token is not known until the file is read.
func (u *Upstream) statusAuth() string {
	if u.Auth == "" && u.Format == config.OpenAI && u.fileTok != nil {
		if cred, err := u.Credential(); err == nil && cred.GrokSession {
			return "grok"
		}
	}
	return u.AuthStyle()
}

func (p *Pool) status(u *Upstream) Status {
	now := p.now()
	s := Status{
		Name: u.Name, URL: u.raw.URL, Format: u.Format, Auth: u.statusAuth(),
		Token: redact(u.Token), TokenFile: u.TokenFile, Model: u.Model, Models: u.Models, MaxTokens: u.MaxTokens,
		Priority: u.Priority, Fallback: u.Fallback, Source: "api", State: "available",
		LastStatus: u.lastStatus, Requests: u.requests, Failures: u.failures,
	}
	if u.static {
		s.Source = "config"
	}
	for name := range u.Headers {
		s.Headers = append(s.Headers, name)
	}
	sort.Strings(s.Headers)
	switch {
	case u.disabled:
		s.State = "disabled"
	case now.Before(u.cooldownUntil):
		s.State = "cooling"
		until := u.cooldownUntil
		s.CooldownUntil, s.CooldownReason = &until, u.reason
	}
	if !u.lastUsed.IsZero() {
		used := u.lastUsed
		s.LastUsed = &used
	}
	return s
}

func redact(token string) string {
	switch {
	case token == "":
		return ""
	case len(token) < 16:
		return "****"
	}
	return token[:6] + "…" + token[len(token)-4:]
}

// Add puts a new upstream in the pool and saves the pool file.
func (p *Pool) Add(raw config.Upstream) (Status, error) {
	resolved, err := resolveNew(raw)
	if err != nil {
		return Status{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.find(raw.Name) != nil {
		return Status{}, ErrExists
	}
	u := p.insert(raw, resolved, false)
	if err := p.save(); err != nil {
		p.remove(u)
		return Status{}, err
	}
	return p.status(u), nil
}

// Update replaces an upstream added through the admin API. An empty token
// keeps the current one. Runtime state (cooldowns, counters) is reset.
func (p *Pool) Update(name string, raw config.Upstream) (Status, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.find(name)
	if old == nil {
		return Status{}, ErrNotFound
	}
	if old.static {
		return Status{}, ErrReadOnly
	}
	raw.Name = name
	if raw.Token == "" {
		raw.Token = old.raw.Token
	}
	resolved, err := resolveNew(raw)
	if err != nil {
		return Status{}, err
	}
	p.remove(old)
	u := p.insert(raw, resolved, false)
	if err := p.save(); err != nil {
		p.remove(u)
		p.add(old)
		return Status{}, err
	}
	return p.status(u), nil
}

// Remove deletes an upstream added through the admin API.
func (p *Pool) Remove(name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	u := p.find(name)
	if u == nil {
		return ErrNotFound
	}
	if u.static {
		return ErrReadOnly
	}
	p.remove(u)
	if err := p.save(); err != nil {
		p.add(u)
		return err
	}
	return nil
}

func (p *Pool) remove(target *Upstream) {
	for i, u := range p.ups {
		if u == target {
			p.ups = append(p.ups[:i], p.ups[i+1:]...)
			return
		}
	}
}

// SetDisabled takes an upstream out of (or back into) rotation. For config
// upstreams this lasts until restart; API upstreams save it to the pool file.
func (p *Pool) SetDisabled(name string, disabled bool) (Status, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	u := p.find(name)
	if u == nil {
		return Status{}, ErrNotFound
	}
	prev := u.disabled
	u.disabled = disabled
	if !u.static {
		u.raw.Disabled = disabled
		if err := p.save(); err != nil {
			u.disabled, u.raw.Disabled = prev, prev
			return Status{}, err
		}
	}
	return p.status(u), nil
}

// Reset clears an upstream's cooldown so it is tried again immediately.
func (p *Pool) Reset(name string) (Status, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	u := p.find(name)
	if u == nil {
		return Status{}, ErrNotFound
	}
	u.cooldownUntil, u.reason = time.Time{}, ""
	return p.status(u), nil
}

type poolFile struct {
	Upstreams []config.Upstream `json:"upstreams"`
}

func resolveNew(raw config.Upstream) (config.Upstream, error) {
	if raw.TokenFile != "" {
		// Letting the admin API point at files would let it send any
		// file on the server to any URL.
		return raw, fmt.Errorf("%w: token_file can only be set in the config file", ErrInvalid)
	}
	resolved, err := raw.Resolve()
	if err == nil {
		err = resolved.Validate()
	}
	if err != nil {
		return resolved, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return resolved, nil
}

func readPoolFile(path string) ([]config.Upstream, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f poolFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f.Upstreams, nil
}

// save writes the API-managed upstreams to the pool file. Caller holds mu.
func (p *Pool) save() error {
	if p.file == "" {
		return nil
	}
	f := poolFile{Upstreams: []config.Upstream{}}
	for _, u := range p.ups {
		if !u.static {
			f.Upstreams = append(f.Upstreams, u.raw)
		}
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	// The file holds tokens: owner-only, written atomically.
	tmp, err := os.CreateTemp(filepath.Dir(p.file), filepath.Base(p.file)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), p.file); err != nil {
		return fmt.Errorf("saving pool file: %w", err)
	}
	return nil
}
