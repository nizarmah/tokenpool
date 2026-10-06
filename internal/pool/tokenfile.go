package pool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
}

func (f *fileToken) get() (string, error) {
	info, err := os.Stat(f.path)
	if err != nil {
		return "", fmt.Errorf("token_file: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.token != "" && info.ModTime().Equal(f.modTime) && info.Size() == f.size {
		return f.token, nil
	}
	data, err := os.ReadFile(f.path)
	if err != nil {
		return "", fmt.Errorf("token_file: %w", err)
	}
	token, err := ParseTokenFile(data, f.field)
	if err != nil {
		return "", fmt.Errorf("token_file %s: %w", f.path, err)
	}
	f.token, f.modTime, f.size = token, info.ModTime(), info.Size()
	return token, nil
}

// defaultTokenFields are tried, in order, in a JSON token file without a
// token_field.
var defaultTokenFields = []string{"access_token", "accessToken", "token"}

// ParseTokenFile extracts a token from a token file: the whole file when
// it holds a bare token, or a string field of a JSON object.
func ParseTokenFile(data []byte, field string) (string, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return "", errors.New("file is empty")
	}
	if data[0] != '{' {
		if field != "" {
			return "", fmt.Errorf("token_field %q needs a JSON file", field)
		}
		if bytes.ContainsAny(data, "\r\n") {
			return "", errors.New("a plain token file must hold one line")
		}
		return string(data), nil
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", errors.New("file starts like JSON but does not parse")
	}
	if field != "" {
		return jsonField(doc, field)
	}
	for _, name := range defaultTokenFields {
		if token, err := jsonField(doc, name); err == nil {
			return token, nil
		}
	}
	return "", fmt.Errorf("no top-level %s field: set token_field", strings.Join(defaultTokenFields, ", "))
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
