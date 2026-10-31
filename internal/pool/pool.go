// Package pool tracks the upstreams tokenpool can send a request to, which
// of them are cooling down after hitting a limit, and which to try next.
package pool

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/nizarmah/tokenpool/internal/config"
	"github.com/nizarmah/tokenpool/internal/quota"
)

var (
	ErrNotFound = errors.New("no upstream with that name")
	ErrExists   = errors.New("an upstream with that name already exists")
	ErrReadOnly = errors.New("upstream is defined in the config file; edit it there")
	ErrInvalid  = errors.New("invalid upstream")
)

// Strategies order the upstreams that can serve a request.
const (
	// Priority tries upstreams by priority, lowest first (also "failover").
	Priority = "priority"
	// RoundRobin spreads load across upstreams that share a priority.
	RoundRobin = "round_robin"
	// Default sends everything the default upstream can serve to it until
	// it is limited, then falls back to priority order.
	Default = "default"
	// MostQuota picks the upstream with the most quota left right now.
	MostQuota = "most_quota"
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
	prio          int                   // runtime priority: config, then admin changes
	minCaller     config.ClientPriority // lowest caller priority served: config, then admin changes
	quota         []quota.Window
	quotaAt       time.Time
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
	// ChatGPT is true when Token is a ChatGPT login's access token, read
	// from a Codex CLI auth.json. AccountID goes with it.
	ChatGPT   bool
	AccountID string
}

// Credential returns the token to send upstream, reading token_file when
// the upstream has one.
func (u *Upstream) Credential() (Credential, error) {
	if u.fileTok == nil {
		return Credential{Token: u.Token}, nil
	}
	tok, err := u.fileTok.get()
	if err != nil {
		return Credential{}, err
	}
	return Credential{Token: tok.token, GrokSession: tok.kind == kindGrok,
		ChatGPT: tok.kind == kindChatGPT, AccountID: tok.accountID}, nil
}

// InvalidateToken makes the next Credential call refresh a ChatGPT login
// even if its access token has not expired yet, after the upstream
// rejected it. It does nothing for other credentials.
func (u *Upstream) InvalidateToken() {
	if u.fileTok != nil {
		u.fileTok.invalidate()
	}
}

// checkToken reads token_file, when the upstream has one, without
// refreshing anything.
func (u *Upstream) checkToken() error {
	if u.fileTok == nil {
		return nil
	}
	return u.fileTok.check()
}

// IsChatGPT reports whether cred is a ChatGPT login for this upstream:
// auth chatgpt, or a Codex auth.json token_file with auth unset.
func (u *Upstream) IsChatGPT(cred Credential) bool {
	return u.Auth == "chatgpt" || (u.Auth == "" && cred.ChatGPT)
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
	// Native is true when the upstream serves the requested model as is,
	// rather than mapping it to another model.
	Native bool
}

// Request describes what a request needs from the pool.
type Request struct {
	// Model is the requested model, after aliases.
	Model string
	// Pinned keeps the request on upstreams that serve Model natively:
	// no upstream may swap it for another model.
	Pinned bool
	// Priority is the caller's priority. An upstream whose
	// min_client_priority ranks above it is skipped. An empty Priority
	// ranks as low.
	Priority config.ClientPriority
	// Prefer names upstreams to try first, in this order (the caller's
	// client_upstreams). It reorders candidates and never adds one.
	Prefer []string
}

// Selection is the result of Pick.
type Selection struct {
	// Candidates are the upstreams to try, in order.
	Candidates []Candidate
	// Unavailable describes eligible upstreams that are disabled or cooling.
	Unavailable []string
	// Excluded names the upstreams that would serve the model but refuse
	// the caller's priority, each with the priority it needs.
	Excluded []string
	// Wait is how long until the soonest cooling upstream is back.
	Wait time.Duration
}

// Pool is safe for concurrent use.
type Pool struct {
	mu       sync.Mutex
	ups      []*Upstream
	seq      int
	strategy string
	def      string // default upstream's name
	rr       uint64
	file     string
	now      func() time.Time

	// Admin choices saved to the pool file, so they outlive a restart.
	savedStrategy string
	savedDefault  *string
	overrides     map[string]override
}

// override is an admin change to an upstream from the config file.
type override struct {
	Priority          *int                   `json:"priority,omitempty"`
	Disabled          *bool                  `json:"disabled,omitempty"`
	MinClientPriority *config.ClientPriority `json:"min_client_priority,omitempty"`
}

// Options configure a pool.
type Options struct {
	Strategy  string
	Default   string // default upstream, for the default strategy
	Upstreams []config.Upstream
	File      string
}

// New builds a pool from the config file's upstreams plus any saved in file.
func New(strategy string, static []config.Upstream, file string) (*Pool, error) {
	return Open(Options{Strategy: strategy, Upstreams: static, File: file})
}

// NormalizeStrategy returns the canonical name of a strategy, accepting
// the older and hyphenated spellings, and false for an unknown one.
func NormalizeStrategy(s string) (string, bool) { return config.NormalizeStrategy(s) }

// Open builds a pool from o: the config file's upstreams, plus the
// upstreams and admin changes saved in o.File.
func Open(o Options) (*Pool, error) {
	strategy, ok := NormalizeStrategy(o.Strategy)
	if !ok {
		return nil, fmt.Errorf("unknown strategy %q", o.Strategy)
	}
	p := &Pool{strategy: strategy, def: o.Default, file: o.File, now: time.Now, overrides: map[string]override{}}
	static, file := o.Upstreams, o.File
	for _, u := range static {
		// Fail at startup on a token_file that can't be read or parsed.
		if err := p.insert(u, u, true).checkToken(); err != nil {
			return nil, fmt.Errorf("upstream %q: %w", u.Name, err)
		}
	}
	if file == "" {
		return p, p.checkDefault()
	}
	saved, err := readPoolFile(file)
	if err != nil {
		return nil, err
	}
	for _, raw := range saved.Upstreams {
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
	if saved.Strategy != "" {
		if p.strategy, ok = NormalizeStrategy(saved.Strategy); !ok {
			return nil, fmt.Errorf("%s: unknown strategy %q", file, saved.Strategy)
		}
		p.savedStrategy = p.strategy
	}
	if saved.Default != nil && (*saved.Default == "" || p.find(*saved.Default) != nil) {
		p.def, p.savedDefault = *saved.Default, saved.Default
	}
	for name, o := range saved.Overrides {
		u := p.find(name)
		if u == nil || !u.static {
			continue // the upstream left the config file
		}
		if o.Priority != nil {
			u.prio = *o.Priority
		}
		if o.Disabled != nil {
			u.disabled = *o.Disabled
		}
		if o.MinClientPriority != nil {
			if !o.MinClientPriority.Valid() {
				return nil, fmt.Errorf("%s: override for %q: min_client_priority must be low, normal or high, got %q",
					file, name, *o.MinClientPriority)
			}
			u.minCaller = *o.MinClientPriority
		}
		p.overrides[name] = o
	}
	p.resort()
	return p, p.checkDefault()
}

func (p *Pool) checkDefault() error {
	if p.def != "" && p.find(p.def) == nil {
		return fmt.Errorf("default upstream %q is not in the pool", p.def)
	}
	return nil
}

// SetClock replaces time.Now, for tests.
func (p *Pool) SetClock(now func() time.Time) { p.now = now }

func (p *Pool) insert(raw, resolved config.Upstream, static bool) *Upstream {
	p.seq++
	u := &Upstream{Upstream: resolved, raw: raw, static: static, seq: p.seq, disabled: resolved.Disabled, prio: resolved.Priority,
		minCaller: resolved.MinClientPriority}
	if resolved.TokenFile != "" {
		u.fileTok = &fileToken{path: resolved.TokenFile, field: resolved.TokenField,
			now: func() time.Time { return p.now() }}
	}
	p.add(u)
	return u
}

// add puts u in the pool, keeping it ordered: primaries before fallbacks,
// then by priority, then by insertion.
func (p *Pool) add(u *Upstream) {
	p.ups = append(p.ups, u)
	p.resort()
}

func (p *Pool) resort() {
	sort.SliceStable(p.ups, func(i, j int) bool {
		a, b := p.ups[i], p.ups[j]
		if a.Fallback != b.Fallback {
			return !a.Fallback
		}
		if a.prio != b.prio {
			return a.prio < b.prio
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
	return p.PickFor(Request{Model: model})
}

// PickFor returns the upstreams that can serve req right now, best first,
// in the order the pool's strategy sets.
func (p *Pool) PickFor(req Request) Selection {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var sel Selection
	var soonest time.Time
	for _, u := range p.ups {
		upstreamModel, ok := u.MapModel(req.Model)
		if !ok {
			continue
		}
		native := upstreamModel == req.Model
		if req.Pinned && !native {
			continue
		}
		if req.Priority.Rank() < u.minCaller.Rank() {
			sel.Excluded = append(sel.Excluded, fmt.Sprintf("%s (needs %s)", u.Name, effectiveMin(u.minCaller)))
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
			sel.Candidates = append(sel.Candidates, Candidate{Upstream: u, UpstreamModel: upstreamModel, Native: native})
		}
	}
	if !soonest.IsZero() {
		sel.Wait = soonest.Sub(now)
	}
	p.order(sel.Candidates, now)
	if p.strategy == RoundRobin {
		p.rr++
		rotateTiers(sel.Candidates, p.rr)
	}
	preferFirst(sel.Candidates, req.Prefer)
	return sel
}

// preferFirst moves the candidates named in prefer to the front, in
// prefer's order, and keeps the others behind them in their order.
func preferFirst(c []Candidate, prefer []string) {
	if len(prefer) == 0 || len(c) < 2 {
		return
	}
	rank := make(map[string]int, len(prefer))
	for i, name := range prefer {
		if _, dup := rank[name]; !dup {
			rank[name] = i
		}
	}
	sort.SliceStable(c, func(i, j int) bool {
		ri, iok := rank[c[i].Name]
		rj, jok := rank[c[j].Name]
		if iok != jok {
			return iok
		}
		return iok && ri < rj
	})
}

// effectiveMin names the caller priority an upstream needs: low when unset.
func effectiveMin(p config.ClientPriority) config.ClientPriority {
	if p == "" {
		return config.PriorityLow
	}
	return p
}

// order sorts candidates for the pool's strategy. Caller holds mu.
//
// Fallbacks always come after every other upstream. Among the rest, the
// default upstream comes first under the default strategy, and the one
// with the most quota left comes first under most_quota. Then lower
// priority, then upstreams that serve the model natively, then config order.
func (p *Pool) order(c []Candidate, now time.Time) {
	left := make(map[*Upstream]int, len(c))
	if p.strategy == MostQuota {
		for _, x := range c {
			left[x.Upstream] = quotaRank(x.quota, now)
		}
	}
	sort.SliceStable(c, func(i, j int) bool {
		a, b := c[i], c[j]
		if p.strategy == Default && p.def != "" && (a.Name == p.def) != (b.Name == p.def) {
			return a.Name == p.def
		}
		if a.Fallback != b.Fallback {
			return !a.Fallback
		}
		if left[a.Upstream] != left[b.Upstream] {
			return left[a.Upstream] > left[b.Upstream]
		}
		if a.prio != b.prio {
			return a.prio < b.prio
		}
		if a.Native != b.Native {
			return a.Native
		}
		return a.seq < b.seq
	})
}

// quotaRank is an upstream's place under most_quota, in whole percents so
// noise doesn't reorder. An upstream that has not reported yet counts as
// full, so it gets tried and reports. One that reports only rate counters
// (an API key) has no quota to compare: it goes after every upstream that
// reports some, and those go by priority among themselves.
func quotaRank(ws []quota.Window, now time.Time) int {
	if len(ws) == 0 {
		return 100
	}
	pct, _, known := quota.Remaining(ws, now)
	if !known {
		return -1
	}
	return int(pct)
}

// rotateTiers rotates each run of candidates sharing a priority (and
// fallback role and nativeness) by n, so load spreads within a tier while
// later tiers stay in reserve. Caller holds mu.
func rotateTiers(c []Candidate, n uint64) {
	for start := 0; start < len(c); {
		end := start + 1
		for end < len(c) && c[end].prio == c[start].prio && c[end].Fallback == c[start].Fallback &&
			c[end].Native == c[start].Native {
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

// Observe records the usage limits an upstream reported in its response
// headers. Headers without any are ignored.
func (p *Pool) Observe(u *Upstream, h http.Header) {
	now := p.now()
	ws := quota.Parse(h, now)
	if len(ws) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	u.quota = quota.Merge(u.quota, ws)
	u.quotaAt = now
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

// Wait returns how long until any upstream that serves req is back, or 0.
func (p *Pool) Wait(req Request) time.Duration {
	sel := p.PickFor(req)
	if len(sel.Candidates) > 0 {
		return 0
	}
	return sel.Wait
}

// Status is the admin view of an upstream. Tokens are redacted.
type Status struct {
	Name      string            `json:"name"`
	URL       string            `json:"url"`
	Format    config.Format     `json:"format"`
	Auth      string            `json:"auth"`
	Token     string            `json:"token,omitempty"`
	TokenFile string            `json:"token_file,omitempty"`
	Model     string            `json:"model,omitempty"`
	Models    map[string]string `json:"models,omitempty"`
	MaxTokens int               `json:"max_tokens,omitempty"`
	Headers   []string          `json:"headers,omitempty"`
	Priority  int               `json:"priority"`
	// MinClientPriority is the lowest caller priority the upstream serves.
	MinClientPriority config.ClientPriority `json:"min_client_priority"`
	Fallback          bool                  `json:"fallback"`
	Default           bool                  `json:"default,omitempty"`
	Source            string                `json:"source"`
	State             string                `json:"state"`
	CooldownUntil     *time.Time            `json:"cooldown_until,omitempty"`
	CooldownReason    string                `json:"cooldown_reason,omitempty"`
	LastStatus        int                   `json:"last_status,omitempty"`
	Requests          int64                 `json:"requests"`
	Failures          int64                 `json:"failures"`
	LastUsed          *time.Time            `json:"last_used,omitempty"`
	Quota             *QuotaStatus          `json:"quota,omitempty"`
}

// QuotaStatus is what an upstream last reported about its usage limits.
type QuotaStatus struct {
	// RemainingPercent is the smallest share left across the limits that
	// apply to every model; Window names that limit and ResetsAt is when
	// it starts over.
	RemainingPercent *float64       `json:"remaining_percent,omitempty"`
	Window           string         `json:"window,omitempty"`
	ResetsAt         *time.Time     `json:"resets_at,omitempty"`
	Windows          []quota.Window `json:"windows"`
	UpdatedAt        time.Time      `json:"updated_at"`
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
		u.fileTok.mu.Lock()
		kind := u.fileTok.cur.kind
		u.fileTok.mu.Unlock()
		switch kind {
		case kindGrok:
			return "grok"
		case kindChatGPT:
			return "chatgpt"
		}
	}
	return u.AuthStyle()
}

func (p *Pool) status(u *Upstream) Status {
	now := p.now()
	s := Status{
		Name: u.Name, URL: u.raw.URL, Format: u.Format, Auth: u.statusAuth(),
		Token: redact(u.Token), TokenFile: u.TokenFile, Model: u.Model, Models: u.Models, MaxTokens: u.MaxTokens,
		Priority: u.prio, MinClientPriority: effectiveMin(u.minCaller), Fallback: u.Fallback, Default: p.strategy == Default && p.def == u.Name, Source: "api", State: "available",
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
	s.Quota = quotaStatus(u, now)
	return s
}

func quotaStatus(u *Upstream, now time.Time) *QuotaStatus {
	if len(u.quota) == 0 {
		return nil
	}
	q := &QuotaStatus{UpdatedAt: u.quotaAt, Windows: make([]quota.Window, 0, len(u.quota))}
	for _, w := range u.quota {
		q.Windows = append(q.Windows, quota.Current(w, now))
	}
	if pct, binding, ok := quota.Remaining(u.quota, now); ok {
		q.RemainingPercent, q.Window, q.ResetsAt = &pct, binding.Name, binding.ResetsAt
	}
	return q
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
	prevDef, prevSavedDef := p.def, p.savedDefault
	if p.def == name {
		none := ""
		p.def, p.savedDefault = "", &none
	}
	if err := p.save(); err != nil {
		p.add(u)
		p.def, p.savedDefault = prevDef, prevSavedDef
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

// SetDisabled pauses an upstream (or resumes it). The change is saved to
// the pool file, when there is one, for config upstreams too.
func (p *Pool) SetDisabled(name string, disabled bool) (Status, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	u := p.find(name)
	if u == nil {
		return Status{}, ErrNotFound
	}
	prev, prevRaw, prevOverride := u.disabled, u.raw.Disabled, p.overrides[name]
	u.disabled = disabled
	if u.static {
		o := p.overrides[name]
		o.Disabled = &disabled
		p.overrides[name] = o
	} else {
		u.raw.Disabled = disabled
	}
	if err := p.save(); err != nil {
		u.disabled, u.raw.Disabled, p.overrides[name] = prev, prevRaw, prevOverride
		return Status{}, err
	}
	return p.status(u), nil
}

// SetPriority changes an upstream's priority and saves it to the pool file.
func (p *Pool) SetPriority(name string, priority int) (Status, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	u := p.find(name)
	if u == nil {
		return Status{}, ErrNotFound
	}
	prev, prevRaw, prevOverride := u.prio, u.raw.Priority, p.overrides[name]
	u.prio = priority
	if u.static {
		o := p.overrides[name]
		o.Priority = &priority
		p.overrides[name] = o
	} else {
		u.raw.Priority = priority
	}
	p.resort()
	if err := p.save(); err != nil {
		u.prio, u.raw.Priority, p.overrides[name] = prev, prevRaw, prevOverride
		p.resort()
		return Status{}, err
	}
	return p.status(u), nil
}

// SetMinClientPriority changes the lowest caller priority an upstream
// serves and saves it to the pool file. An empty value means low: everyone.
func (p *Pool) SetMinClientPriority(name string, min config.ClientPriority) (Status, error) {
	min = config.NormalizePriority(min)
	if min != "" && !min.Valid() {
		return Status{}, fmt.Errorf("%w: min_client_priority must be low, normal or high, got %q", ErrInvalid, min)
	}
	if min == "" {
		min = config.PriorityLow
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	u := p.find(name)
	if u == nil {
		return Status{}, ErrNotFound
	}
	prev, prevRaw, prevOverride := u.minCaller, u.raw.MinClientPriority, p.overrides[name]
	u.minCaller = min
	if u.static {
		o := p.overrides[name]
		o.MinClientPriority = &min
		p.overrides[name] = o
	} else {
		u.raw.MinClientPriority = min
	}
	if err := p.save(); err != nil {
		u.minCaller, u.raw.MinClientPriority, p.overrides[name] = prev, prevRaw, prevOverride
		return Status{}, err
	}
	return p.status(u), nil
}

// SetDefault makes name the default upstream and switches the pool to the
// default strategy. An empty name clears the default, and a pool on the
// default strategy goes back to priority. Saved to the pool file.
func (p *Pool) SetDefault(name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if name != "" && p.find(name) == nil {
		return ErrNotFound
	}
	prevDef, prevStrategy, prevSavedDef, prevSavedStrategy := p.def, p.strategy, p.savedDefault, p.savedStrategy
	p.def, p.savedDefault = name, &name
	switch {
	case name != "":
		p.strategy, p.savedStrategy = Default, Default
	case p.strategy == Default:
		p.strategy, p.savedStrategy = Priority, Priority
	}
	if err := p.save(); err != nil {
		p.def, p.strategy, p.savedDefault, p.savedStrategy = prevDef, prevStrategy, prevSavedDef, prevSavedStrategy
		return err
	}
	return nil
}

// SetStrategy changes how the pool orders upstreams and saves it to the
// pool file.
func (p *Pool) SetStrategy(strategy string) error {
	canonical, ok := NormalizeStrategy(strategy)
	if !ok {
		return fmt.Errorf("%w: strategy must be priority, round_robin, default or most_quota, got %q", ErrInvalid, strategy)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	prev, prevSaved := p.strategy, p.savedStrategy
	p.strategy, p.savedStrategy = canonical, canonical
	if err := p.save(); err != nil {
		p.strategy, p.savedStrategy = prev, prevSaved
		return err
	}
	return nil
}

// Summary is a short view of the whole pool: how it routes and how much
// quota each upstream has left.
type Summary struct {
	Strategy        string `json:"strategy"`
	DefaultUpstream string `json:"default_upstream,omitempty"`
	// First is the upstream a request every upstream can serve would go
	// to right now.
	First     string       `json:"first,omitempty"`
	Upstreams []SummaryRow `json:"upstreams"`
}

// SummaryRow is one upstream in a Summary, in the strategy's order.
type SummaryRow struct {
	Name              string                `json:"name"`
	State             string                `json:"state"`
	Priority          int                   `json:"priority"`
	MinClientPriority config.ClientPriority `json:"min_client_priority"`
	Fallback          bool                  `json:"fallback,omitempty"`
	Default           bool                  `json:"default,omitempty"`
	RemainingPercent  *float64              `json:"remaining_percent,omitempty"`
	Window            string                `json:"window,omitempty"`
	ResetsAt          *time.Time            `json:"resets_at,omitempty"`
	CooldownUntil     *time.Time            `json:"cooldown_until,omitempty"`
	CooldownReason    string                `json:"cooldown_reason,omitempty"`
	Requests          int64                 `json:"requests"`
	Failures          int64                 `json:"failures"`
}

// Summarize returns the pool's Summary.
func (p *Pool) Summarize() Summary {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	all := make([]Candidate, 0, len(p.ups))
	for _, u := range p.ups {
		all = append(all, Candidate{Upstream: u, Native: true})
	}
	p.order(all, now)
	sum := Summary{Strategy: p.strategy, Upstreams: make([]SummaryRow, 0, len(all))}
	if p.strategy == Default {
		sum.DefaultUpstream = p.def
	}
	for _, c := range all {
		st := p.status(c.Upstream)
		row := SummaryRow{Name: st.Name, State: st.State, Priority: st.Priority, MinClientPriority: st.MinClientPriority, Fallback: st.Fallback,
			Default: st.Default, CooldownUntil: st.CooldownUntil, CooldownReason: st.CooldownReason,
			Requests: st.Requests, Failures: st.Failures}
		if q := st.Quota; q != nil {
			row.RemainingPercent, row.Window, row.ResetsAt = q.RemainingPercent, q.Window, q.ResetsAt
		}
		if sum.First == "" && st.State == "available" {
			sum.First = st.Name
		}
		sum.Upstreams = append(sum.Upstreams, row)
	}
	return sum
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

// poolFile is what the pool file holds: upstreams added through the admin
// API, and admin changes to the strategy, the default and config upstreams.
type poolFile struct {
	Upstreams []config.Upstream   `json:"upstreams"`
	Strategy  string              `json:"strategy,omitempty"`
	Default   *string             `json:"default_upstream,omitempty"`
	Overrides map[string]override `json:"overrides,omitempty"`
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

func readPoolFile(path string) (poolFile, error) {
	var f poolFile
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// save writes the API-managed upstreams to the pool file. Caller holds mu.
func (p *Pool) save() error {
	if p.file == "" {
		return nil
	}
	f := poolFile{Upstreams: []config.Upstream{}, Strategy: p.savedStrategy, Default: p.savedDefault}
	for _, u := range p.ups {
		if !u.static {
			f.Upstreams = append(f.Upstreams, u.raw)
		}
	}
	for name, o := range p.overrides {
		if u := p.find(name); u != nil && u.static {
			if f.Overrides == nil {
				f.Overrides = map[string]override{}
			}
			f.Overrides[name] = o
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
