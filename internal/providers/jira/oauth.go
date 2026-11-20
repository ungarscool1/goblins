package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// DefaultTokenURL is Atlassian's OAuth 2.0 token endpoint.
const DefaultTokenURL = "https://auth.atlassian.com/oauth/token" //nolint:gosec // a public URL

// OAuth is Jira Cloud's OAuth 2.0 (3LO) for a person who authorized this installation's app.
// Atlassian rotates refresh tokens: each refresh returns a new one and retires the old, so
// it is kept in a file of its own, written before the new access token is used.
type OAuth struct {
	TokenURL     string
	ClientID     string
	ClientSecret core.Secret
	// RefreshFile holds the current refresh token, readable by the service only.
	RefreshFile string
	HTTP        *http.Client
	Now         func() time.Time

	mu      sync.Mutex
	access  core.Secret
	expires time.Time
}

// Apply implements Auth.
func (o *OAuth) Apply(ctx context.Context, req *http.Request) error {
	tok, err := o.token(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok.Reveal())
	return nil
}

func (o *OAuth) token(ctx context.Context) (core.Secret, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now()
	if !o.access.Empty() && now.Before(o.expires.Add(-time.Minute)) {
		return o.access, nil
	}
	refresh, err := os.ReadFile(o.RefreshFile)
	if err != nil {
		return core.Secret{}, core.Errorf(core.KindAuthRequired, "jira.oauth",
			"no refresh token in the configured file: authorize the app again")
	}
	body, _ := json.Marshal(map[string]string{"grant_type": "refresh_token",
		"client_id": o.ClientID, "client_secret": o.ClientSecret.Reveal(),
		"refresh_token": strings.TrimSpace(string(refresh))})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.tokenURL(),
		bytes.NewReader(body))
	if err != nil {
		return core.Secret{}, core.Wrap(core.KindInternal, "jira.oauth", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client().Do(req)
	if err != nil {
		return core.Secret{}, core.Errorf(core.KindTransient, "jira.oauth",
			"cannot reach the token endpoint")
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 == 4 {
		return core.Secret{}, core.Errorf(core.KindAuthRequired, "jira.oauth",
			"the refresh token was refused: authorize the app again")
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if resp.StatusCode/100 != 2 || json.Unmarshal(data, &out) != nil || out.AccessToken == "" {
		return core.Secret{}, core.Errorf(core.KindTransient, "jira.oauth",
			"the token endpoint failed")
	}
	if out.RefreshToken != "" {
		if err := writePrivate(o.RefreshFile, out.RefreshToken); err != nil {
			return core.Secret{}, core.Wrap(core.KindInternal, "jira.oauth", err)
		}
	}
	o.access = core.NewSecret(out.AccessToken)
	o.expires = now.Add(time.Duration(max(out.ExpiresIn, 60)) * time.Second)
	return o.access, nil
}

func (o *OAuth) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *OAuth) tokenURL() string {
	if o.TokenURL != "" {
		return o.TokenURL
	}
	return DefaultTokenURL
}

func (o *OAuth) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// writePrivate replaces a file atomically, readable by its owner only, so a crash leaves the
// old token or the new one, never half of one.
func writePrivate(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".refresh-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(content + "\n"); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
