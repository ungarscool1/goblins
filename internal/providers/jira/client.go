// Package jira adapts Jira to the tracker and event source contracts. It acts as the person
// whose credentials it is given, never as an admin-created bot, and it needs no webhook: it
// polls. It reads by default, and writes to Jira only when the configuration says so.
package jira

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

const (
	typeName    = "jira"
	maxResponse = 8 << 20
	maxPages    = 20
	pageSize    = 100
)

// Deployment is the kind of Jira site. Cloud and Data Center share REST API version 2, but
// not search, identities or permission lookups.
type Deployment string

const (
	DeploymentAuto       Deployment = ""
	DeploymentCloud      Deployment = "cloud"
	DeploymentDataCenter Deployment = "data_center"
)

// Auth adds credentials to a request. It is the only place they are seen.
type Auth interface {
	Apply(ctx context.Context, req *http.Request) error
}

// BasicToken is a Jira Cloud API token used with its owner's email. It acts with all of that
// person's access, so a site's policy has to permit it.
type BasicToken struct {
	Email string
	Token core.Secret
}

// Apply implements Auth.
func (b BasicToken) Apply(_ context.Context, req *http.Request) error {
	raw := b.Email + ":" + b.Token.Reveal()
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(raw)))
	return nil
}

// Bearer is an OAuth access token or a Data Center personal access token.
type Bearer struct{ Token core.Secret }

// Apply implements Auth.
func (b Bearer) Apply(_ context.Context, req *http.Request) error {
	req.Header.Set("Authorization", "Bearer "+b.Token.Reveal())
	return nil
}

// Client talks to Jira's REST API version 2.
type Client struct {
	base       string
	auth       Auth
	http       *http.Client
	now        func() time.Time
	mu         sync.Mutex
	deployment Deployment
	me         *user
}

// ClientOptions configures NewClient.
type ClientOptions struct {
	// URL is the site, such as https://example.atlassian.net, or the OAuth API base,
	// https://api.atlassian.com/ex/jira/<cloud id>.
	URL        string
	Auth       Auth
	Deployment Deployment
	HTTP       *http.Client
	Now        func() time.Time
}

// NewClient checks the options.
func NewClient(o ClientOptions) (*Client, error) {
	u, err := url.Parse(o.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !isLoopback(u.Hostname())) {
		return nil, core.Errorf(core.KindInvalid, "jira.client",
			"url must be an https URL (plain http only on loopback, for tests)")
	}
	if u.User != nil || u.RawQuery != "" {
		return nil, core.Errorf(core.KindInvalid, "jira.client",
			"url cannot carry credentials or a query")
	}
	if o.Auth == nil {
		return nil, core.Errorf(core.KindInvalid, "jira.client", "credentials are required")
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Client{base: strings.TrimRight(u.String(), "/"), auth: o.Auth, http: o.HTTP,
		now: o.Now, deployment: o.Deployment}, nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// Deployment finds out whether the site is Cloud or Data Center, once.
func (c *Client) Deployment(ctx context.Context) (Deployment, error) {
	c.mu.Lock()
	d := c.deployment
	c.mu.Unlock()
	if d != DeploymentAuto {
		return d, nil
	}
	var info struct {
		DeploymentType string `json:"deploymentType"`
	}
	if err := c.get(ctx, "/rest/api/2/serverInfo", nil, &info); err != nil {
		return "", err
	}
	d = DeploymentDataCenter
	if strings.EqualFold(info.DeploymentType, "Cloud") {
		d = DeploymentCloud
	}
	c.mu.Lock()
	c.deployment = d
	c.mu.Unlock()
	return d, nil
}

// Myself is the account the credentials belong to, and so the one Goblins writes as.
func (c *Client) Myself(ctx context.Context) (user, error) {
	c.mu.Lock()
	me := c.me
	c.mu.Unlock()
	if me != nil {
		return *me, nil
	}
	var u user
	if err := c.get(ctx, "/rest/api/2/myself", nil, &u); err != nil {
		return user{}, err
	}
	c.mu.Lock()
	c.me = &u
	c.mu.Unlock()
	return u, nil
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return c.call(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	op := "jira." + strings.ToLower(method)
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return core.Wrap(core.KindInternal, op, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return core.Wrap(core.KindInternal, op, err)
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Jira refuses state-changing requests that look like they come from a browser form.
	req.Header.Set("X-Atlassian-Token", "no-check")
	if err := c.auth.Apply(ctx, req); err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return core.Wrap(core.KindCancelled, op, ctx.Err())
		}
		return core.Wrap(core.KindTransient, op, errors.New("Jira cannot be reached"))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return core.Wrap(core.KindTransient, op, err)
	}
	if resp.StatusCode/100 != 2 {
		return classify(op, resp, data, c.now())
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return core.Wrap(core.KindTransient, op, fmt.Errorf("unreadable response: %w", err))
	}
	return nil
}

// classify turns a Jira refusal into a kind the workflow can act on, with a message saying
// what a person has to do. Jira's own text is not repeated: it can echo request content.
func classify(op string, resp *http.Response, data []byte, now time.Time) error {
	text := strings.ToLower(string(data))
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		err := core.Errorf(core.KindRateLimited, op, "Jira is rate limiting these credentials")
		if after, ok := retryAfter(resp.Header.Get("Retry-After"), now); ok {
			return core.WithRetryAt(err, after)
		}
		return err
	case resp.Header.Get("X-Seraph-LoginReason") == "AUTHENTICATION_DENIED":
		return core.Errorf(core.KindAuthRequired, op,
			"Jira wants a person to log in in a browser first (CAPTCHA), "+
				"then credentials work again")
	case resp.StatusCode == http.StatusUnauthorized:
		return core.Errorf(core.KindAuthRequired, op,
			"Jira refused the credentials: they expired or were revoked. Renew them to continue")
	case resp.StatusCode == http.StatusForbidden && strings.Contains(text, "api") &&
		(strings.Contains(text, "disabled") || strings.Contains(text, "not allowed")):
		return core.Errorf(core.KindUnsupported, op,
			"the Jira site does not allow this kind of API access for these credentials")
	case resp.StatusCode == http.StatusForbidden:
		return core.Errorf(core.KindForbidden, op, "the Jira account is not allowed to do this")
	case resp.StatusCode == http.StatusNotFound:
		return core.Errorf(core.KindNotFound, op, "not found, or not visible to the Jira account")
	case resp.StatusCode == http.StatusConflict:
		return core.Errorf(core.KindConflict, op, "Jira reported a conflict")
	case resp.StatusCode == http.StatusBadRequest:
		return core.Errorf(core.KindInvalid, op, "Jira rejected the request")
	case resp.StatusCode >= 500:
		return core.Errorf(core.KindTransient, op, "Jira answered %d", resp.StatusCode)
	}
	return core.Errorf(core.KindInternal, op, "unexpected Jira answer %d", resp.StatusCode)
}

func retryAfter(v string, now time.Time) (time.Time, bool) {
	if v == "" {
		return time.Time{}, false
	}
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
		return now.Add(time.Duration(n) * time.Second), true
	}
	if t, err := http.ParseTime(v); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// user is a Jira account. Cloud identifies people by account ID; Data Center by key and
// user name.
type user struct {
	AccountID   string `json:"accountId,omitempty"`
	AccountType string `json:"accountType,omitempty"`
	Key         string `json:"key,omitempty"`
	Name        string `json:"name,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
}

// id is the account's stable identity. A display name never is: anyone can take one.
func (u user) id() string {
	if u.AccountID != "" {
		return u.AccountID
	}
	return u.Key
}

// login is what people and the audit log see. On Cloud it is the account ID, as Cloud has no
// unique user name; on Data Center, the user name.
func (u user) login() string {
	if u.Name != "" {
		return u.Name
	}
	return u.AccountID
}

func (u user) actor() core.Actor {
	a := core.Actor{ID: u.id(), Login: u.login(), Type: core.ActorHuman}
	if u.AccountType == "app" {
		a.Type = core.ActorBot
	}
	return a
}

// jiraTime parses Jira's timestamps, such as 2026-11-19T10:04:05.123+0100.
func jiraTime(s string) time.Time {
	for _, layout := range []string{"2006-01-02T15:04:05.000-0700", time.RFC3339Nano} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
