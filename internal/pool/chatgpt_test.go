package pool

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nizarmah/tokenpool/internal/config"
)

// fakeJWT builds an unsigned JWT with the given claims.
func fakeJWT(claims map[string]any) string {
	enc := base64.RawURLEncoding
	payload, _ := json.Marshal(claims)
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString(payload) + ".sig"
}

func codexAuthJSON(access, refresh, accountID string) string {
	tokens := map[string]any{
		"id_token":      fakeJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-from-id"}}),
		"access_token":  access,
		"refresh_token": refresh,
	}
	if accountID != "" {
		tokens["account_id"] = accountID
	}
	doc, _ := json.Marshal(map[string]any{
		"OPENAI_API_KEY": nil,
		"tokens":         tokens,
		"last_refresh":   "2026-10-20T10:00:00Z",
		"keep_me":        map[string]any{"n": 12345678901234},
	})
	return string(doc)
}

// tokenServer is a fake auth.openai.com/oauth/token.
type tokenServer struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]string
	status int
	answer map[string]any
}

func newTokenServer(t *testing.T) *tokenServer {
	ts := &tokenServer{status: http.StatusOK}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		ts.mu.Lock()
		ts.bodies = append(ts.bodies, body)
		status, answer := ts.status, ts.answer
		ts.mu.Unlock()
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "want JSON", http.StatusUnsupportedMediaType)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(ts.Close)
	prev := ChatGPTTokenURL
	ChatGPTTokenURL = ts.URL + "/oauth/token"
	t.Cleanup(func() { ChatGPTTokenURL = prev })
	return ts
}

func (ts *tokenServer) calls() []map[string]string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]map[string]string(nil), ts.bodies...)
}

func codexPool(t *testing.T, path string, now time.Time) *Upstream {
	t.Helper()
	u := config.Upstream{Name: "codex", URL: config.ChatGPTCodexURL, Format: config.OpenAI, Auth: "chatgpt", TokenFile: path}
	p, err := New("priority", []config.Upstream{u}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.SetClock(func() time.Time { return now })
	return p.Pick("gpt-5-codex").Candidates[0].Upstream
}

func TestParseCodexAuthJSON(t *testing.T) {
	access := fakeJWT(map[string]any{"exp": 1_800_000_000})
	parsed, err := parseTokenFile([]byte(codexAuthJSON(access, "rt-1", "acct-1")), "")
	if err != nil || parsed.kind != kindChatGPT || parsed.token != access || parsed.refresh != "rt-1" ||
		parsed.accountID != "acct-1" || !parsed.expires.Equal(time.Unix(1_800_000_000, 0)) {
		t.Fatalf("parsed = %+v, %v", parsed, err)
	}
	// Older files carry the account only in the ID token.
	parsed, _ = parseTokenFile([]byte(codexAuthJSON(access, "rt-1", "")), "")
	if parsed.accountID != "acct-from-id" {
		t.Errorf("account from id_token = %q", parsed.accountID)
	}
	// An API-key Codex login is a plain bearer token.
	parsed, err = parseTokenFile([]byte(`{"OPENAI_API_KEY":"sk-proj-abc","tokens":null}`), "")
	if err != nil || parsed.kind != kindPlain || parsed.token != "sk-proj-abc" {
		t.Errorf("API-key login = %+v, %v", parsed, err)
	}
	_, err = parseTokenFile([]byte(`{"tokens":{"access_token":"","refresh_token":"secret-value"}}`), "")
	if err == nil || !strings.Contains(err.Error(), "codex login") || strings.Contains(err.Error(), "secret-value") {
		t.Errorf("empty access token: %v", err)
	}
}

func TestChatGPTTokenRefreshesWhenExpired(t *testing.T) {
	ts := newTokenServer(t)
	now := time.Unix(1_800_000_000, 0)
	newAccess := fakeJWT(map[string]any{"exp": now.Add(10 * 24 * time.Hour).Unix()})
	ts.answer = map[string]any{"access_token": newAccess, "refresh_token": "rt-2", "id_token": "id-2"}

	path := filepath.Join(t.TempDir(), "auth.json")
	expired := fakeJWT(map[string]any{"exp": now.Add(time.Minute).Unix()}) // inside the refresh margin
	if err := os.WriteFile(path, []byte(codexAuthJSON(expired, "rt-1", "acct-1")), 0o644); err != nil {
		t.Fatal(err)
	}
	u := codexPool(t, path, now)
	if len(ts.calls()) != 0 {
		t.Fatal("startup must not refresh")
	}

	cred, err := u.Credential()
	if err != nil {
		t.Fatal(err)
	}
	if cred.Token != newAccess || !cred.ChatGPT || cred.AccountID != "acct-1" || !u.IsChatGPT(cred) {
		t.Errorf("credential = %+v", cred)
	}
	calls := ts.calls()
	if len(calls) != 1 || calls[0]["grant_type"] != "refresh_token" || calls[0]["refresh_token"] != "rt-1" ||
		calls[0]["client_id"] != ChatGPTClientID {
		t.Fatalf("refresh requests = %v", calls)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("auth.json mode = %v, want 0600", info.Mode().Perm())
	}
	var doc map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	tokens := doc["tokens"].(map[string]any)
	if tokens["access_token"] != newAccess || tokens["refresh_token"] != "rt-2" || tokens["id_token"] != "id-2" ||
		tokens["account_id"] != "acct-1" {
		t.Errorf("tokens written back = %v", tokens)
	}
	if doc["last_refresh"] != now.UTC().Format(time.RFC3339Nano) || !strings.Contains(string(data), "12345678901234") {
		t.Errorf("file after refresh = %s", data)
	}
	if leftovers, _ := filepath.Glob(path + ".*.tmp"); len(leftovers) != 0 {
		t.Errorf("temp files left: %v", leftovers)
	}

	// The fresh token is used without refreshing again.
	if cred, _ := u.Credential(); cred.Token != newAccess || len(ts.calls()) != 1 {
		t.Errorf("second call refreshed again or changed token")
	}
}

func TestChatGPTTokenRefreshAfterRejection(t *testing.T) {
	ts := newTokenServer(t)
	now := time.Unix(1_800_000_000, 0)
	ts.answer = map[string]any{"access_token": "fresh-access"}
	path := filepath.Join(t.TempDir(), "auth.json")
	valid := fakeJWT(map[string]any{"exp": now.Add(time.Hour).Unix()})
	if err := os.WriteFile(path, []byte(codexAuthJSON(valid, "rt-1", "acct-1")), 0o600); err != nil {
		t.Fatal(err)
	}
	u := codexPool(t, path, now)
	if cred, _ := u.Credential(); cred.Token != valid {
		t.Fatalf("token = %q", cred.Token)
	}
	u.InvalidateToken()
	cred, err := u.Credential()
	if err != nil || cred.Token != "fresh-access" {
		t.Fatalf("after invalidate: %q, %v", cred.Token, err)
	}
	// The refresh token was not rotated, so it stays.
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"rt-1"`) {
		t.Errorf("refresh token lost: %s", data)
	}
}

func TestChatGPTRefreshFailure(t *testing.T) {
	ts := newTokenServer(t)
	now := time.Unix(1_800_000_000, 0)
	ts.status = http.StatusBadRequest
	ts.answer = map[string]any{"error": "refresh_token_reused", "echo": "secret-value"}
	path := filepath.Join(t.TempDir(), "auth.json")

	// Still valid for a moment: the old token is used.
	soon := fakeJWT(map[string]any{"exp": now.Add(2 * time.Minute).Unix()})
	if err := os.WriteFile(path, []byte(codexAuthJSON(soon, "rt-1", "acct-1")), 0o600); err != nil {
		t.Fatal(err)
	}
	u := codexPool(t, path, now)
	if cred, err := u.Credential(); err != nil || cred.Token != soon {
		t.Fatalf("near expiry: %q, %v", cred.Token, err)
	}

	// Expired: the error says why, without the body.
	expired := fakeJWT(map[string]any{"exp": now.Add(-time.Minute).Unix()})
	if err := os.WriteFile(path, []byte(codexAuthJSON(expired, "rt-1", "acct-1")), 0o600); err != nil {
		t.Fatal(err)
	}
	future := now.Add(time.Second)
	_ = os.Chtimes(path, future, future)
	_, err := u.Credential()
	if err == nil || !strings.Contains(err.Error(), "refresh_token_reused") || !strings.Contains(err.Error(), "codex login") ||
		strings.Contains(err.Error(), "secret-value") || strings.Contains(err.Error(), "rt-1") {
		t.Errorf("err = %v", err)
	}
}

func TestChatGPTAuthDetectedFromFile(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "auth.json")
	access := fakeJWT(map[string]any{"exp": now.Add(time.Hour).Unix()})
	if err := os.WriteFile(path, []byte(codexAuthJSON(access, "rt", "acct")), 0o600); err != nil {
		t.Fatal(err)
	}
	u := config.Upstream{Name: "codex", URL: config.ChatGPTCodexURL, Format: config.OpenAI, TokenFile: path}
	p, err := New("priority", []config.Upstream{u}, "")
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := p.Get("codex"); st.Auth != "chatgpt" {
		t.Errorf("auth = %q", st.Auth)
	}
	c := p.Pick("gpt-5").Candidates[0]
	cred, _ := c.Credential()
	if !c.IsChatGPT(cred) {
		t.Error("a Codex auth.json should be sent as a ChatGPT login")
	}
}
