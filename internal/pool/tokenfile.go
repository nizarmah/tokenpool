package pool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// fileToken reads an upstream's token from a file and re-reads it whenever
// the file changes, so a token another tool keeps refreshed stays current.
// Errors never include the file's contents.
type fileToken struct {
	path, field string

	mu      sync.Mutex
	modTime time.Time
	size    int64
	token   string
	grok    bool
}

func (f *fileToken) get() (string, bool, error) {
	info, err := os.Stat(f.path)
	if err != nil {
		return "", false, fmt.Errorf("token_file: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.token != "" && info.ModTime().Equal(f.modTime) && info.Size() == f.size {
		return f.token, f.grok, nil
	}
	data, err := os.ReadFile(f.path)
	if err != nil {
		return "", false, fmt.Errorf("token_file: %w", err)
	}
	token, grok, err := ParseTokenFile(data, f.field)
	if err != nil {
		return "", false, fmt.Errorf("token_file %s: %w", f.path, err)
	}
	f.token, f.grok, f.modTime, f.size = token, grok, info.ModTime(), info.Size()
	return token, grok, nil
}

// defaultTokenFields are tried, in order, in a JSON token file without a
// token_field.
var defaultTokenFields = []string{"access_token", "accessToken", "token"}

// ParseTokenFile extracts a token from a token file: the whole file when
// it holds a bare token, a string field of a JSON object, or the session
// key inside a Grok ~/.grok/auth.json. grok is true for that last shape.
func ParseTokenFile(data []byte, field string) (token string, grok bool, err error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return "", false, errors.New("file is empty")
	}
	if data[0] != '{' {
		if field != "" {
			return "", false, fmt.Errorf("token_field %q needs a JSON file", field)
		}
		if bytes.ContainsAny(data, "\r\n") {
			return "", false, errors.New("a plain token file must hold one line")
		}
		return string(data), false, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", false, errors.New("file starts like JSON but does not parse")
	}
	if field != "" {
		if token, ok, err := grokSessionKey(doc, field); ok || err != nil {
			return token, ok, err
		}
		token, err := jsonField(doc, field)
		return token, false, err
	}
	for _, name := range defaultTokenFields {
		if token, err := jsonField(doc, name); err == nil {
			return token, false, nil
		}
	}
	if token, ok, err := grokSessionKey(doc, ""); ok || err != nil {
		return token, ok, err
	}
	return "", false, fmt.Errorf("no top-level %s field: set token_field", strings.Join(defaultTokenFields, ", "))
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
