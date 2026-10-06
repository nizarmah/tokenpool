// Package config loads and validates tokenpool's YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Format is the wire API an upstream speaks.
type Format string

const (
	// Anthropic is the Messages API: POST {url}/v1/messages.
	Anthropic Format = "anthropic"
	// OpenAI is the Chat Completions API: POST {url}/chat/completions.
	OpenAI Format = "openai"
)

// Upstream is one entry in the pool: any URL with any token.
type Upstream struct {
	// Name identifies the upstream in logs, headers and the admin API.
	Name string `yaml:"name" json:"name"`
	// URL is the API base, e.g. https://api.anthropic.com or https://api.x.ai/v1.
	URL string `yaml:"url" json:"url"`
	// Format is the API the upstream speaks: anthropic or openai.
	Format Format `yaml:"format" json:"format"`
	// Token is the credential sent upstream, written inline or as ${ENV_VAR}.
	Token string `yaml:"token,omitempty" json:"token,omitempty"`
	// TokenFile reads the credential from a file instead, re-reading it
	// whenever the file changes, for tokens another tool keeps refreshed.
	// Config file only: the admin API can't set it.
	TokenFile string `yaml:"token_file,omitempty" json:"token_file,omitempty"`
	// TokenField picks the token out of a JSON token file, as a dot path
	// such as "tokens.access_token". Without it, tokenpool looks for a
	// top-level access_token, accessToken or token.
	TokenField string `yaml:"token_field,omitempty" json:"token_field,omitempty"`
	// Auth says how the token is sent: bearer, x-api-key, header:<Name> or none.
	// Defaults to x-api-key for anthropic and bearer for openai.
	Auth string `yaml:"auth,omitempty" json:"auth,omitempty"`
	// Model, when set, replaces the requested model for every request.
	// With Models set, it covers models that match no pattern.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
	// Models maps requested models (exact or with * wildcards) to upstream
	// models. An empty value passes the requested model through. When set
	// without Model, requests for unmatched models skip this upstream.
	Models map[string]string `yaml:"models,omitempty" json:"models,omitempty"`
	// MaxTokens caps max_tokens sent to this upstream (0 means no cap).
	MaxTokens int `yaml:"max_tokens,omitempty" json:"max_tokens,omitempty"`
	// Headers are extra request headers. ${ENV_VAR} references are expanded.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	// Priority orders the pool: lower values are tried first.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`
	// Fallback marks an upstream that only takes traffic once every
	// non-fallback upstream is limited or failing, whatever its priority.
	Fallback bool `yaml:"fallback,omitempty" json:"fallback,omitempty"`
	// Disabled keeps the upstream in the pool without sending it traffic.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// Cooldowns say how long an upstream sits out after each kind of failure,
// when the upstream does not say itself (Retry-After, rate-limit resets).
type Cooldowns struct {
	RateLimit  time.Duration `yaml:"rate_limit"`
	Overloaded time.Duration `yaml:"overloaded"`
	Error      time.Duration `yaml:"error"`
	Auth       time.Duration `yaml:"auth"`
	Quota      time.Duration `yaml:"quota"`
	Max        time.Duration `yaml:"max"`
}

// Config is the whole tokenpool configuration file.
type Config struct {
	Listen string `yaml:"listen"`
	// Strategy picks among upstreams of equal priority: failover or round_robin.
	Strategy string `yaml:"strategy"`
	// ClientKeys maps a caller's name (a person or an app) to the key it
	// calls tokenpool with.
	ClientKeys map[string]string `yaml:"client_keys"`
	// AllowAnonymous serves requests without a client key. Only for local use.
	AllowAnonymous bool `yaml:"allow_anonymous"`
	// AdminKey enables the /admin API. Leave empty to disable it.
	AdminKey string `yaml:"admin_key"`
	// PoolFile stores upstreams added through the admin API.
	PoolFile string `yaml:"pool_file"`
	// DefaultMaxTokens fills max_tokens when an OpenAI request goes to an
	// Anthropic upstream without one (Anthropic requires it).
	DefaultMaxTokens int           `yaml:"default_max_tokens"`
	MaxBodyBytes     int64         `yaml:"max_body_bytes"`
	ConnectTimeout   time.Duration `yaml:"connect_timeout"`
	HeaderTimeout    time.Duration `yaml:"header_timeout"`
	Cooldowns        Cooldowns     `yaml:"cooldowns"`
	Upstreams        []Upstream    `yaml:"upstreams"`
}

// Load reads, expands, defaults and validates the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.PoolFile != "" && !filepath.IsAbs(cfg.PoolFile) {
		cfg.PoolFile = filepath.Join(filepath.Dir(path), cfg.PoolFile)
	}
	for i, u := range cfg.Upstreams {
		if u.TokenFile != "" && !filepath.IsAbs(u.TokenFile) {
			cfg.Upstreams[i].TokenFile = filepath.Join(filepath.Dir(path), u.TokenFile)
		}
	}
	return cfg, nil
}

// Parse decodes a config document. Unknown fields are errors.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if err := cfg.resolve(); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) resolve() error {
	var err error
	if c.AdminKey, err = Expand(c.AdminKey); err != nil {
		return fmt.Errorf("admin_key: %w", err)
	}
	for name, key := range c.ClientKeys {
		if c.ClientKeys[name], err = Expand(key); err != nil {
			return fmt.Errorf("client_keys.%s: %w", name, err)
		}
	}
	for i, u := range c.Upstreams {
		if c.Upstreams[i], err = u.Resolve(); err != nil {
			return fmt.Errorf("upstream %q: %w", u.Name, err)
		}
	}
	return nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.Strategy == "" {
		c.Strategy = "failover"
	}
	if c.DefaultMaxTokens == 0 {
		c.DefaultMaxTokens = 8192
	}
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = 64 << 20
	}
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = 10 * time.Second
	}
	if c.HeaderTimeout == 0 {
		c.HeaderTimeout = 10 * time.Minute
	}
	cd := &c.Cooldowns
	setDefault(&cd.RateLimit, time.Minute)
	setDefault(&cd.Overloaded, 15*time.Second)
	setDefault(&cd.Error, 30*time.Second)
	setDefault(&cd.Auth, 10*time.Minute)
	setDefault(&cd.Quota, time.Hour)
	setDefault(&cd.Max, 24*time.Hour)
}

func setDefault(d *time.Duration, v time.Duration) {
	if *d == 0 {
		*d = v
	}
}

func (c *Config) validate() error {
	if c.Strategy != "failover" && c.Strategy != "round_robin" {
		return fmt.Errorf("strategy must be failover or round_robin, got %q", c.Strategy)
	}
	if len(c.ClientKeys) == 0 && !c.AllowAnonymous {
		return errors.New("no client_keys: add one per person or app (tokenpool keygen), " +
			"or set allow_anonymous: true for local use")
	}
	seenKeys := map[string]string{}
	for name, key := range c.ClientKeys {
		if err := checkKey("client_keys."+name, key); err != nil {
			return err
		}
		if other, ok := seenKeys[key]; ok {
			return fmt.Errorf("client_keys.%s and client_keys.%s share a key", name, other)
		}
		seenKeys[key] = name
	}
	if c.AdminKey != "" {
		if err := checkKey("admin_key", c.AdminKey); err != nil {
			return err
		}
		if _, ok := seenKeys[c.AdminKey]; ok {
			return errors.New("admin_key must differ from every client key")
		}
	}
	seen := map[string]bool{}
	for _, u := range c.Upstreams {
		if err := u.Validate(); err != nil {
			return err
		}
		if seen[u.Name] {
			return fmt.Errorf("upstream %q is defined twice", u.Name)
		}
		seen[u.Name] = true
	}
	return nil
}

// minKeyLen keeps client and admin keys hard to guess. tokenpool keygen
// makes 35-character keys.
const minKeyLen = 16

func checkKey(field, key string) error {
	switch {
	case isPlaceholder(key):
		return fmt.Errorf("%s is still a placeholder: replace %s with a real key (tokenpool keygen)", field, key)
	case len(key) < minKeyLen:
		return fmt.Errorf("%s is too short: use at least %d characters (tokenpool keygen makes one)", field, minKeyLen)
	}
	return nil
}

// isPlaceholder spots a <...> value copied from the example config.
func isPlaceholder(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">")
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Validate checks one upstream entry. Resolve it first.
func (u Upstream) Validate() error {
	if !namePattern.MatchString(u.Name) {
		return fmt.Errorf("upstream name %q must be letters, digits, '.', '_' or '-'", u.Name)
	}
	parsed, err := url.Parse(u.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("upstream %q: url must be an http(s) URL, got %q", u.Name, u.URL)
	}
	if u.Format != Anthropic && u.Format != OpenAI {
		return fmt.Errorf("upstream %q: format must be anthropic or openai, got %q", u.Name, u.Format)
	}
	if isPlaceholder(u.Token) {
		return fmt.Errorf("upstream %q: token is still a placeholder: replace %s with the real key", u.Name, u.Token)
	}
	switch {
	case u.Token != "" && u.TokenFile != "":
		return fmt.Errorf("upstream %q: set token or token_file, not both", u.Name)
	case u.TokenField != "" && u.TokenFile == "":
		return fmt.Errorf("upstream %q: token_field needs token_file", u.Name)
	case isPlaceholder(u.TokenFile):
		return fmt.Errorf("upstream %q: token_file is still a placeholder", u.Name)
	}
	for name, value := range u.Headers {
		if isPlaceholder(value) {
			return fmt.Errorf("upstream %q: headers.%s is still a placeholder", u.Name, name)
		}
	}
	switch auth := u.AuthStyle(); {
	case auth == "bearer", auth == "x-api-key", auth == "none":
	case strings.HasPrefix(auth, "header:") && len(auth) > len("header:"):
	default:
		return fmt.Errorf("upstream %q: auth must be bearer, x-api-key, header:<Name> or none, got %q", u.Name, auth)
	}
	if u.MaxTokens < 0 {
		return fmt.Errorf("upstream %q: max_tokens must not be negative", u.Name)
	}
	return nil
}

// AuthStyle returns how the token is sent, applying the per-format default.
func (u Upstream) AuthStyle() string {
	if u.Auth != "" {
		return u.Auth
	}
	if u.Format == Anthropic {
		return "x-api-key"
	}
	return "bearer"
}

// Resolve returns a copy with ${ENV_VAR} references expanded.
func (u Upstream) Resolve() (Upstream, error) {
	var err error
	if u.URL, err = Expand(u.URL); err != nil {
		return u, fmt.Errorf("url: %w", err)
	}
	if u.Token, err = Expand(u.Token); err != nil {
		return u, fmt.Errorf("token: %w", err)
	}
	if u.TokenFile, err = Expand(u.TokenFile); err != nil {
		return u, fmt.Errorf("token_file: %w", err)
	}
	if len(u.Headers) > 0 {
		headers := make(map[string]string, len(u.Headers))
		for k, v := range u.Headers {
			if headers[k], err = Expand(v); err != nil {
				return u, fmt.Errorf("headers.%s: %w", k, err)
			}
		}
		u.Headers = headers
	}
	return u, nil
}

// MapModel returns the model to send upstream for a requested model, and
// false when this upstream should not serve it.
func (u Upstream) MapModel(requested string) (string, bool) {
	pick := func(v string) string {
		if v == "" {
			return requested
		}
		return v
	}
	if v, ok := u.Models[requested]; ok {
		return pick(v), true
	}
	best, bestPattern, found := "", "", false
	for pattern, v := range u.Models {
		if !strings.Contains(pattern, "*") || !Glob(pattern, requested) {
			continue
		}
		// The most specific pattern (most literal characters) wins.
		if !found || literalLen(pattern) > literalLen(bestPattern) ||
			(literalLen(pattern) == literalLen(bestPattern) && pattern < bestPattern) {
			best, bestPattern, found = v, pattern, true
		}
	}
	switch {
	case found:
		return pick(best), true
	case u.Model != "":
		return u.Model, true
	case len(u.Models) > 0:
		return "", false
	}
	return requested, true
}

func literalLen(pattern string) int {
	return len(pattern) - strings.Count(pattern, "*")
}

// Glob matches s against pattern, where * matches any run of characters.
func Glob(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(s, part)
		if i < 0 {
			return false
		}
		s = s[i+len(part):]
	}
	return strings.HasSuffix(s, last)
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Expand replaces ${NAME} with the environment variable NAME. A reference
// to an unset variable is an error, so a missing secret never goes out empty.
func Expand(s string) (string, error) {
	var missing []string
	out := envRef.ReplaceAllStringFunc(s, func(ref string) string {
		name := envRef.FindStringSubmatch(ref)[1]
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("environment variable %s is not set", strings.Join(missing, ", "))
	}
	return out, nil
}
