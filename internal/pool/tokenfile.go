package pool

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type tokenKind int

const (
	kindPlain   tokenKind = iota // an API key or bearer token
	kindGrok                     // a Grok ~/.grok/auth.json session
	kindChatGPT                  // a Codex CLI auth.json ChatGPT login
)

// fileToken reads an upstream's token from a file and re-reads it whenever
// the file changes, so a token another tool keeps refreshed stays current.
// A ChatGPT login is refreshed here when its access token expires, and the
// new tokens are written back to the file. Errors never include the file's
// contents.
type fileToken struct {
	path, field string

	mu      sync.Mutex
	modTime time.Time
	size    int64
	cur     parsedToken
	stale   bool // the upstream rejected the access token: refresh it
	now     func() time.Time
}

// parsedToken is what a token file holds.
type parsedToken struct {
	token     string
	kind      tokenKind
	accountID string // ChatGPT logins
	refresh   string // ChatGPT logins
	expires   time.Time
	refreshed time.Time      // ChatGPT logins: last_refresh
	doc       map[string]any // ChatGPT logins: the whole file, for write-back
}

func (f *fileToken) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

// load reads the file if it changed since the last read. Caller holds mu.
func (f *fileToken) load() error {
	info, err := os.Stat(f.path)
	if err != nil {
		return fmt.Errorf("token_file: %w", err)
	}
	if f.cur.token != "" && info.ModTime().Equal(f.modTime) && info.Size() == f.size {
		return nil
	}
	data, err := os.ReadFile(f.path)
	if err != nil {
		return fmt.Errorf("token_file: %w", err)
	}
	parsed, err := parseTokenFile(data, f.field)
	if err != nil {
		return fmt.Errorf("token_file %s: %w", f.path, err)
	}
	if parsed.refresh != f.cur.refresh || parsed.token != f.cur.token {
		f.stale = false // someone else refreshed it
	}
	f.cur, f.modTime, f.size = parsed, info.ModTime(), info.Size()
	return nil
}

// check reads and parses the file without refreshing anything, to fail
// at startup on a file that can't be used.
func (f *fileToken) check() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.load()
}

func (f *fileToken) get() (parsedToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.load(); err != nil {
		return parsedToken{}, err
	}
	if f.cur.kind == kindChatGPT && (f.stale || needsRefresh(f.cur, f.clock())) {
		if err := f.refreshChatGPT(); err != nil {
			if f.stale || !f.cur.expires.After(f.clock()) {
				return parsedToken{}, err
			}
			// The access token still works for a little while: use it,
			// and try refreshing again on the next request.
		}
	}
	return f.cur, nil
}

func (f *fileToken) invalidate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cur.kind == kindChatGPT {
		f.stale = true
	}
}

// defaultTokenFields are tried, in order, in a JSON token file without a
// token_field.
var defaultTokenFields = []string{"access_token", "accessToken", "token"}

// ParseTokenFile extracts a token from a token file: the whole file when
// it holds a bare token, a string field of a JSON object, the session key
// inside a Grok ~/.grok/auth.json (grok is true then), or the access token
// of a Codex CLI auth.json ChatGPT login.
func ParseTokenFile(data []byte, field string) (token string, grok bool, err error) {
	parsed, err := parseTokenFile(data, field)
	return parsed.token, parsed.kind == kindGrok, err
}

func parseTokenFile(data []byte, field string) (parsedToken, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return parsedToken{}, errors.New("file is empty")
	}
	if data[0] != '{' {
		if field != "" {
			return parsedToken{}, fmt.Errorf("token_field %q needs a JSON file", field)
		}
		if bytes.ContainsAny(data, "\r\n") {
			return parsedToken{}, errors.New("a plain token file must hold one line")
		}
		return parsedToken{token: string(data)}, nil
	}
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return parsedToken{}, errors.New("file starts like JSON but does not parse")
	}
	if field != "" {
		if token, ok, err := grokSessionKey(doc, field); ok || err != nil {
			return parsedToken{token: token, kind: kindGrok}, err
		}
		token, err := jsonField(doc, field)
		return parsedToken{token: token}, err
	}
	for _, name := range defaultTokenFields {
		if token, err := jsonField(doc, name); err == nil {
			return parsedToken{token: token}, nil
		}
	}
	if parsed, ok, err := chatGPTLogin(doc); ok || err != nil {
		return parsed, err
	}
	if token, ok, err := grokSessionKey(doc, ""); ok || err != nil {
		return parsedToken{token: token, kind: kindGrok}, err
	}
	return parsedToken{}, fmt.Errorf("no top-level %s field: set token_field", strings.Join(defaultTokenFields, ", "))
}

// chatGPTLogin reads a Codex CLI auth.json ($CODEX_HOME/auth.json). A
// ChatGPT login has a tokens object; an API-key login has OPENAI_API_KEY.
// ok is false when doc is neither.
func chatGPTLogin(doc map[string]any) (parsedToken, bool, error) {
	tokens, isLogin := doc["tokens"].(map[string]any)
	if !isLogin {
		if key, err := jsonField(doc, "OPENAI_API_KEY"); err == nil {
			return parsedToken{token: key}, true, nil
		}
		return parsedToken{}, false, nil
	}
	access, _ := tokens["access_token"].(string)
	refresh, _ := tokens["refresh_token"].(string)
	if _, hasRefresh := tokens["refresh_token"]; !hasRefresh {
		return parsedToken{}, false, nil
	}
	access, refresh = strings.TrimSpace(access), strings.TrimSpace(refresh)
	if access == "" || refresh == "" {
		return parsedToken{}, true, errors.New("codex auth.json has no access_token or refresh_token: run codex login again")
	}
	p := parsedToken{token: access, kind: kindChatGPT, refresh: refresh, doc: doc}
	p.accountID, _ = tokens["account_id"].(string)
	if p.accountID == "" {
		// Older files only carry it inside the ID token.
		if idToken, ok := tokens["id_token"].(string); ok {
			if claims, ok := jwtClaims(idToken); ok {
				if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
					p.accountID, _ = auth["chatgpt_account_id"].(string)
				}
			}
		}
	}
	if claims, ok := jwtClaims(access); ok {
		if exp, ok := claims["exp"].(json.Number); ok {
			if n, err := exp.Int64(); err == nil {
				p.expires = time.Unix(n, 0)
			}
		}
	}
	if s, ok := doc["last_refresh"].(string); ok {
		p.refreshed, _ = time.Parse(time.RFC3339Nano, s)
	}
	return p, true, nil
}

// jwtClaims decodes a JWT's payload without checking its signature: the
// claims only say when to refresh, and the provider checks the token.
func jwtClaims(token string) (map[string]any, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, false
	}
	var claims map[string]any
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if dec.Decode(&claims) != nil {
		return nil, false
	}
	return claims, true
}

// grokSessionKey reads the `key` of a Grok login session. field, when set,
// is the session's name (the object's top-level key, which itself contains
// dots, so it is not a dot path). ok is false when doc is not a Grok
// auth.json. An error with ok false is not used; an error with ok true
// means the file is a Grok auth.json that cannot yield one session.
func grokSessionKey(doc map[string]any, field string) (string, bool, error) {
	sessions := map[string]string{}
	for name, raw := range doc {
		obj, ok := raw.(map[string]any)
		if !ok || !grokSession(name, obj) {
			continue
		}
		key, _ := obj["key"].(string)
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		sessions[name] = key
	}
	if len(sessions) == 0 {
		return "", false, nil
	}
	if field != "" {
		token, ok := sessions[field]
		if !ok {
			return "", false, nil
		}
		return token, true, nil
	}
	if len(sessions) == 1 {
		for _, token := range sessions {
			return token, true, nil
		}
	}
	names := make([]string, 0, len(sessions))
	for name := range sessions {
		names = append(names, name)
	}
	sort.Strings(names)
	return "", true, fmt.Errorf("grok auth.json has %d sessions (%s): set token_field to one", len(names), strings.Join(names, ", "))
}

// grokSession reports a Grok auth.json session object: a login scope name
// such as "https://auth.x.ai::…" or an object carrying auth_mode / refresh_token.
func grokSession(name string, obj map[string]any) bool {
	if _, ok := obj["key"].(string); !ok {
		return false
	}
	if strings.Contains(name, "://") {
		return true
	}
	_, mode := obj["auth_mode"]
	_, refresh := obj["refresh_token"]
	return mode || refresh
}

func jsonField(doc map[string]any, path string) (string, error) {
	var cur any = doc
	for _, key := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return "", fmt.Errorf("no field %q", path)
		}
		if cur, ok = obj[key]; !ok {
			return "", fmt.Errorf("no field %q", path)
		}
	}
	token, ok := cur.(string)
	if !ok || strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("field %q is not a non-empty string", path)
	}
	return strings.TrimSpace(token), nil
}
