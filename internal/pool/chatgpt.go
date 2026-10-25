package pool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// The ChatGPT login refresh, as the Codex CLI does it
// (codex-rs/login/src/auth/manager.rs). Tests point ChatGPTTokenURL at a
// fake.
var (
	ChatGPTTokenURL = "https://auth.openai.com/oauth/token"
	ChatGPTClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	RefreshClient   = &http.Client{Timeout: 30 * time.Second}
)

const (
	// refreshBefore refreshes an access token this long before it expires.
	refreshBefore = 5 * time.Minute
	// refreshEvery refreshes a token whose expiry can't be read once it
	// is this old, like the Codex CLI.
	refreshEvery = 8 * 24 * time.Hour
)

func needsRefresh(p parsedToken, now time.Time) bool {
	if !p.expires.IsZero() {
		return p.expires.Sub(now) < refreshBefore
	}
	return !p.refreshed.IsZero() && now.Sub(p.refreshed) > refreshEvery
}

// refreshChatGPT trades the refresh token for new tokens and writes them
// back to the file, keeping everything else in it. Caller holds f.mu.
func (f *fileToken) refreshChatGPT() error {
	body, _ := json.Marshal(map[string]string{
		"client_id":     ChatGPTClientID,
		"grant_type":    "refresh_token",
		"refresh_token": f.cur.refresh,
	})
	req, err := http.NewRequest(http.MethodPost, ChatGPTTokenURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := RefreshClient.Do(req)
	if err != nil {
		return fmt.Errorf("refreshing the ChatGPT login: %w", err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		// The body can echo credentials: report only the error code.
		code := oauthErrorCode(data)
		msg := fmt.Sprintf("refreshing the ChatGPT login failed: HTTP %d", res.StatusCode)
		if code != "" {
			msg += " (" + code + ")"
		}
		if res.StatusCode == http.StatusBadRequest || res.StatusCode == http.StatusUnauthorized {
			msg += ": sign in again with codex login"
		}
		return errors.New(msg)
	}
	var got struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if json.Unmarshal(data, &got) != nil || got.AccessToken == "" {
		return errors.New("refreshing the ChatGPT login: the answer has no access_token")
	}

	doc := f.cur.doc
	tokens, _ := doc["tokens"].(map[string]any)
	tokens["access_token"] = got.AccessToken
	if got.IDToken != "" {
		tokens["id_token"] = got.IDToken
	}
	if got.RefreshToken != "" {
		tokens["refresh_token"] = got.RefreshToken
	}
	doc["last_refresh"] = f.clock().UTC().Format(time.RFC3339Nano)
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(f.path, append(out, '\n')); err != nil {
		return fmt.Errorf("saving the refreshed ChatGPT login: %w", err)
	}
	f.stale = false
	f.cur.token = "" // force a re-read of what was written
	return f.load()
}

// oauthErrorCode reads "error" (a string, or an object with a code) from
// an OAuth error answer.
func oauthErrorCode(data []byte) string {
	var shape struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &shape) != nil || len(shape.Error) == 0 {
		return ""
	}
	var code string
	if json.Unmarshal(shape.Error, &code) == nil {
		return code
	}
	var obj struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(shape.Error, &obj)
	return obj.Code
}

// writeFileAtomic replaces path with data, owner-only, so a reader never
// sees half a file and no other user can read the tokens.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
