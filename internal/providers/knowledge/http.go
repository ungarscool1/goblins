package knowledge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// client is a small JSON client for the wiki APIs, which classifies refusals the same way.
type client struct {
	base string
	auth string
	http *http.Client
	op   string
}

func newClient(op, base, auth string) (*client, error) {
	u, err := url.Parse(base)
	local := err == nil && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1")
	if err != nil || u.Host == "" || (u.Scheme != "https" && !local) || u.User != nil {
		return nil, core.Errorf(core.KindInvalid, op,
			"url must be an https URL without credentials")
	}
	return &client{base: strings.TrimRight(base, "/"), auth: auth, op: op,
		http: &http.Client{Timeout: 30 * time.Second}}, nil
}

func basic(user, token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+token))
}

func (c *client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return core.Wrap(core.KindInternal, c.op, err)
	}
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return core.Errorf(core.KindTransient, c.op, "cannot reach the knowledge source")
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return core.Errorf(core.KindAuthRequired, c.op, "the credentials were refused or expired")
	case resp.StatusCode == http.StatusForbidden:
		return core.Errorf(core.KindForbidden, c.op, "the account may not read this")
	case resp.StatusCode == http.StatusTooManyRequests:
		return core.Errorf(core.KindRateLimited, c.op, "rate limited")
	case resp.StatusCode/100 != 2:
		return core.Errorf(core.KindTransient, c.op, "answered %d", resp.StatusCode)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return core.Errorf(core.KindTransient, c.op, "unreadable answer")
	}
	return nil
}

var tags = regexp.MustCompile(`(?s)<[^>]*>`)

// plain turns stored HTML into text. It is for reading, not rendering: nothing of the markup
// survives to be interpreted.
func plain(s string) string {
	s = tags.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(html.UnescapeString(s)), " ")
}

// words quotes nothing and passes letters and digits only, which every query language takes
// as plain words.
var plainWord = regexp.MustCompile(`^[a-z0-9]+$`)

func words(terms []string) []string {
	var out []string
	for _, t := range terms {
		if plainWord.MatchString(t) {
			out = append(out, t)
		}
	}
	return out
}

func since(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func plural(n int, w string) string { return fmt.Sprintf("%d %s", n, w) }
