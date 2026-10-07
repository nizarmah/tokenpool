package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	t.Setenv("TP_TEST_KEY", "client-secret-0123456789")
	t.Setenv("TP_TEST_CLAUDE", "sk-ant-api-test")
	dir := t.TempDir()
	path := filepath.Join(dir, "tokenpool.yaml")
	writeFile(t, path, `
client_keys:
  alice: ${TP_TEST_KEY}
pool_file: pool.json
cooldowns:
  rate_limit: 2m
upstreams:
  - name: claude
    url: https://api.anthropic.com
    format: anthropic
    token: ${TP_TEST_CLAUDE}
  - name: grok
    url: https://api.x.ai/v1
    format: openai
    token: literal-$not-expanded
    model: grok-4
  - name: grok-file
    url: https://api.x.ai/v1
    format: openai
    token_file: secrets/grok.json
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ClientKeys["alice"]; got != "client-secret-0123456789" {
		t.Errorf("client key = %q", got)
	}
	if got := cfg.Upstreams[0].Token; got != "sk-ant-api-test" {
		t.Errorf("claude token = %q", got)
	}
	if got := cfg.Upstreams[1].Token; got != "literal-$not-expanded" {
		t.Errorf("grok token = %q, want $ without braces left alone", got)
	}
	if got := cfg.Upstreams[2].TokenFile; got != filepath.Join(dir, "secrets", "grok.json") {
		t.Errorf("token_file = %q, want it resolved next to the config", got)
	}
	if cfg.PoolFile != filepath.Join(dir, "pool.json") {
		t.Errorf("pool_file = %q, want it next to the config", cfg.PoolFile)
	}
	if cfg.Cooldowns.RateLimit != 2*time.Minute || cfg.Cooldowns.Quota != time.Hour {
		t.Errorf("cooldowns = %+v", cfg.Cooldowns)
	}
	if cfg.Listen != ":8080" || cfg.Strategy != "failover" || cfg.DefaultMaxTokens != 8192 {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	if cfg.Upstreams[0].AuthStyle() != "x-api-key" || cfg.Upstreams[1].AuthStyle() != "bearer" {
		t.Error("default auth styles wrong")
	}
}

func TestAnthropicCredentialChoice(t *testing.T) {
	api, err := Parse([]byte(`
allow_anonymous: true
upstreams:
  - {name: api, url: https://api.anthropic.com, format: anthropic, token: sk-ant-api03-example}
  - {name: sub, url: https://api.anthropic.com, format: anthropic, token: sk-ant-oat01-example}
  - {name: forced, url: https://api.anthropic.com, format: anthropic, token: sk-ant-oat01-example, auth: x-api-key}
  - {name: explicit, url: https://api.anthropic.com, format: anthropic, token: refreshed-later, auth: setup-token}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := api.Upstreams[0].AuthStyle(); got != "x-api-key" {
		t.Errorf("api key auth = %q", got)
	}
	if got := api.Upstreams[1].AuthStyle(); got != "setup-token" {
		t.Errorf("setup-token prefix auth = %q", got)
	}
	if got := api.Upstreams[2].AuthStyle(); got != "x-api-key" {
		t.Errorf("explicit x-api-key = %q", got)
	}
	if got := api.Upstreams[3].AuthFor("from-file"); got != "setup-token" {
		t.Errorf("explicit setup-token = %q", got)
	}
	fileToken := configUpstreamAuth(t, `
allow_anonymous: true
upstreams:
  - {name: file, url: https://api.anthropic.com, format: anthropic, token_file: /tmp/tok}
`)
	if got := fileToken.AuthFor("sk-ant-oat01-from-file"); got != "setup-token" {
		t.Errorf("token_file setup-token auth = %q", got)
	}
	if got := fileToken.AuthFor("sk-ant-api03-from-file"); got != "x-api-key" {
		t.Errorf("token_file api key auth = %q", got)
	}
	if _, err := Parse([]byte(`allow_anonymous: true
upstreams:
  - {name: a, url: https://api.x.ai/v1, format: openai, auth: setup-token}`)); err == nil {
		t.Error("setup-token on an openai upstream was accepted")
	}
	if _, err := Parse([]byte(`allow_anonymous: true
upstreams:
  - {name: a, url: https://api.anthropic.com, format: anthropic, auth: grok}`)); err == nil {
		t.Error("grok auth on an anthropic upstream was accepted")
	}
}

func TestTokenFileTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	path := filepath.Join(dir, "tokenpool.yaml")
	writeFile(t, path, `
allow_anonymous: true
upstreams:
  - name: grok
    url: https://cli-chat-proxy.grok.com/v1
    format: openai
    token_file: ~/.grok/auth.json
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".grok", "auth.json")
	if cfg.Upstreams[0].TokenFile != want {
		t.Errorf("token_file = %q, want %q", cfg.Upstreams[0].TokenFile, want)
	}
}

func configUpstreamAuth(t *testing.T, doc string) Upstream {
	t.Helper()
	cfg, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Upstreams[0]
}

func TestParseErrors(t *testing.T) {
	tests := map[string]string{
		"no client keys":       `upstreams: []`,
		"unset env var":        "client_keys: {a: ${TP_TEST_UNSET_VAR}}",
		"unknown field":        "allow_anonymous: true\nlisten_addr: :1",
		"bad format":           "allow_anonymous: true\nupstreams: [{name: a, url: 'http://x', format: grpc}]",
		"bad url":              "allow_anonymous: true\nupstreams: [{name: a, url: 'x.com', format: openai}]",
		"bad name":             "allow_anonymous: true\nupstreams: [{name: 'a b', url: 'http://x', format: openai}]",
		"duplicate name":       "allow_anonymous: true\nupstreams: [{name: a, url: 'http://x', format: openai}, {name: a, url: 'http://y', format: openai}]",
		"bad auth":             "allow_anonymous: true\nupstreams: [{name: a, url: 'http://x', format: openai, auth: basic}]",
		"bad strategy":         "allow_anonymous: true\nstrategy: random",
		"shared key":           "client_keys: {a: kkkkkkkkkkkkkkkkkk, b: kkkkkkkkkkkkkkkkkk}",
		"admin=client key":     "client_keys: {a: kkkkkkkkkkkkkkkkkk}\nadmin_key: kkkkkkkkkkkkkkkkkk",
		"short client key":     "client_keys: {a: tooshort}",
		"short admin key":      "client_keys: {a: kkkkkkkkkkkkkkkkkk}\nadmin_key: short",
		"placeholder key":      "client_keys: {a: '<tokenpool keygen output>'}",
		"token and token_file": "allow_anonymous: true\nupstreams: [{name: a, url: 'http://x', format: openai, token: t, token_file: /f}]",
		"token_field alone":    "allow_anonymous: true\nupstreams: [{name: a, url: 'http://x', format: openai, token_field: x}]",
		"placeholder token":    "allow_anonymous: true\nupstreams: [{name: a, url: 'http://x', format: openai, token: '<xAI API key>'}]",
		"chat completions url": "allow_anonymous: true\nupstreams: [{name: a, url: 'https://api.x.ai/v1/chat/completions', format: openai}]",
	}
	for name, doc := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(doc)); err == nil {
				t.Errorf("Parse(%q) succeeded, want error", doc)
			}
		})
	}
}

func TestMapModel(t *testing.T) {
	u := Upstream{Models: map[string]string{
		"claude-*":         "grok-4-fast",
		"claude-opus-*":    "grok-4",
		"exact":            "mapped",
		"keep-*":           "",
		"*-haiku-*":        "small",
		"claude-*-haiku-*": "smaller",
	}}
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"claude-opus-5-5", "grok-4", true},
		{"claude-sonnet-5", "grok-4-fast", true},
		{"claude-3-5-haiku-latest", "smaller", true},
		{"exact", "mapped", true},
		{"keep-this", "keep-this", true},
		{"gpt-4o", "", false},
	}
	for _, tt := range tests {
		got, ok := u.MapModel(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("MapModel(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}

	withDefault := Upstream{Model: "fallback", Models: map[string]string{"a": "b"}}
	if got, ok := withDefault.MapModel("zzz"); got != "fallback" || !ok {
		t.Errorf("default model: got %q, %v", got, ok)
	}
	passthrough := Upstream{}
	if got, ok := passthrough.MapModel("anything"); got != "anything" || !ok {
		t.Errorf("passthrough: got %q, %v", got, ok)
	}
}

func TestGlob(t *testing.T) {
	tests := []struct {
		pattern, s string
		want       bool
	}{
		{"*", "", true},
		{"a*", "abc", true},
		{"*c", "abc", true},
		{"a*c", "abc", true},
		{"a*c", "ab", false},
		{"a*b*c", "a-b-c", true},
		{"a*b*c", "a-c-b", false},
		{"meta-llama/*", "meta-llama/Llama-3", true},
		{"ab*ba", "aba", false},
	}
	for _, tt := range tests {
		if got := Glob(tt.pattern, tt.s); got != tt.want {
			t.Errorf("Glob(%q, %q) = %v", tt.pattern, tt.s, got)
		}
	}
}

func TestExampleConfigParses(t *testing.T) {
	data, err := os.ReadFile("../../tokenpool.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(data); err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("example with <...> values left in: err = %v, want a placeholder error", err)
	}
	n := 0
	filled := regexp.MustCompile(`"<[^>]*>"`).ReplaceAllFunc(data, func([]byte) []byte {
		n++
		return []byte(fmt.Sprintf("filled-in-value-%04d", n))
	})
	cfg, err := Parse(filled)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, u := range cfg.Upstreams {
		got = append(got, fmt.Sprintf("%s/%d/%v", u.Name, u.Priority, u.Fallback))
	}
	if want := "claude-api-1/1/false claude-api-2/2/false grok-api/3/true"; strings.Join(got, " ") != want {
		t.Errorf("example upstreams = %v, want %s", got, want)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
