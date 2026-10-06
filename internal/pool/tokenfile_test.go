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

func TestParseTokenFile(t *testing.T) {
	tests := []struct {
		name, data, field, want, err string
	}{
		{name: "bare token", data: "tok-123\n", want: "tok-123"},
		{name: "access_token", data: `{"access_token":"a1","refresh_token":"r1"}`, want: "a1"},
		{name: "accessToken", data: `{"accessToken":"a2"}`, want: "a2"},
		{name: "token", data: `{"token":" a3 "}`, want: "a3"},
		{name: "nested field", data: `{"tokens":{"access":"a4"}}`, field: "tokens.access", want: "a4"},
		{name: "missing field", data: `{"tokens":{}}`, field: "tokens.access", err: `no field "tokens.access"`},
		{name: "not a string", data: `{"token":5}`, field: "token", err: "not a non-empty string"},
		{name: "no default field", data: `{"id_token":"x"}`, err: "set token_field"},
		{name: "empty", data: "  \n", err: "empty"},
		{name: "two lines", data: "a\nb", err: "one line"},
		{name: "field on plain file", data: "tok", field: "token", err: "needs a JSON file"},
		{name: "broken JSON", data: `{"token": "secret-value"`, err: "does not parse"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, grok, err := ParseTokenFile([]byte(tt.data), tt.field)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
				if strings.Contains(err.Error(), "secret-value") {
					t.Fatal("error leaks file contents")
				}
				return
			}
			if err != nil || got != tt.want || grok {
				t.Fatalf("got %q grok %v, %v; want %q", got, grok, err, tt.want)
			}
		})
	}
}

func TestParseGrokAuthJSON(t *testing.T) {
	one := `{"https://auth.x.ai::client":{"key":"eyJ-session","auth_mode":"oidc","refresh_token":"r"}}`
	got, grok, err := ParseTokenFile([]byte(one), "")
	if err != nil || !grok || got != "eyJ-session" {
		t.Fatalf("got %q grok %v err %v", got, grok, err)
	}

	two := `{
	  "https://auth.x.ai::a": {"key":"secret-a","auth_mode":"oidc"},
	  "https://auth.x.ai::b": {"key":"secret-b","auth_mode":"oidc"}
	}`
	_, _, err = ParseTokenFile([]byte(two), "")
	if err == nil || !strings.Contains(err.Error(), "https://auth.x.ai::a") || strings.Contains(err.Error(), "secret-") {
		t.Fatalf("err = %v", err)
	}
	got, grok, err = ParseTokenFile([]byte(two), "https://auth.x.ai::b")
	if err != nil || !grok || got != "secret-b" {
		t.Fatalf("selected %q grok %v err %v", got, grok, err)
	}

	// A normal token file with a nested object named key is not a Grok login.
	got, grok, err = ParseTokenFile([]byte(`{"access_token":"a1","extra":{"key":"nope"}}`), "")
	if err != nil || grok || got != "a1" {
		t.Fatalf("access_token got %q grok %v err %v", got, grok, err)
	}
}

func TestTokenFileFollowsChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	write := func(s string, mod time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	write(`{"access_token":"first"}`, now)

	u := up("grok", 0)
	u.Token, u.TokenFile = "", path
	p, err := New("failover", []config.Upstream{u}, "")
	if err != nil {
		t.Fatal(err)
	}
	cand := p.Pick("m").Candidates[0]
	if tok, err := cand.AuthToken(); tok != "first" || err != nil {
		t.Fatalf("token = %q, %v", tok, err)
	}
	write(`{"access_token":"second-token"}`, now.Add(time.Minute))
	if tok, _ := cand.AuthToken(); tok != "second-token" {
		t.Errorf("after rewrite: token = %q", tok)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := cand.AuthToken(); err == nil || !strings.Contains(err.Error(), "token_file") {
		t.Errorf("missing file: err = %v", err)
	}
	if st, _ := p.Get("grok"); st.Token != "" || st.TokenFile != path {
		t.Errorf("status token = %q, file = %q", st.Token, st.TokenFile)
	}
}

func TestTokenFileChecks(t *testing.T) {
	bad := up("grok", 0)
	bad.Token, bad.TokenFile = "", filepath.Join(t.TempDir(), "missing.json")
	if _, err := New("failover", []config.Upstream{bad}, ""); err == nil {
		t.Error("want a startup error for an unreadable token_file")
	}

	p, _ := New("failover", nil, "")
	viaAPI := up("sneaky", 0)
	viaAPI.Token, viaAPI.TokenFile = "", "/etc/passwd"
	if _, err := p.Add(viaAPI); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "config file") {
		t.Errorf("admin API token_file: err = %v", err)
	}
}
