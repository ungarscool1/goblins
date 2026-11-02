// Package github adapts GitHub to the tracker, SCM and event source contracts. It
// authenticates as a GitHub App, using installation tokens that are short lived and
// scoped to one repository. GitHub credentials are unrelated to model credentials.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
)

const (
	defaultAPI  = "https://api.github.com"
	maxResponse = 16 << 20
	maxPages    = 20
	apiVersion  = "2022-11-28"
	tokenSlack  = 2 * time.Minute
	typeName    = "github"
)

// Client talks to the REST API as a GitHub App.
type Client struct {
	api    string
	appID  string
	key    *rsa.PrivateKey
	http   *http.Client
	now    func() time.Time
	mu     sync.Mutex
	tokens map[string]installationToken
	login  string
}

type installationToken struct {
	value     core.Secret
	expiresAt time.Time
}

// ClientOptions configures NewClient.
type ClientOptions struct {
	API        string
	AppID      string
	PrivateKey []byte
	HTTP       *http.Client
	Now        func() time.Time
}

// NewClient validates the App credentials.
func NewClient(o ClientOptions) (*Client, error) {
	if o.AppID == "" {
		return nil, core.Errorf(core.KindInvalid, "github.client", "app_id is required")
	}
	key, err := parseKey(o.PrivateKey)
	if err != nil {
		return nil, err
	}
	if o.API == "" {
		o.API = defaultAPI
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Client{api: strings.TrimRight(o.API, "/"), appID: o.AppID, key: key, http: o.HTTP, now: o.Now, tokens: map[string]installationToken{}}, nil
}

func parseKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, core.Errorf(core.KindInvalid, "github.client", "private key is not valid PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, core.Errorf(core.KindInvalid, "github.client", "private key cannot be parsed")
	}
	k, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, core.Errorf(core.KindInvalid, "github.client", "private key is not RSA")
	}
	return k, nil
}

// AppLogin returns the account the App's comments appear under. It never changes while
// the App exists, so it is read once.
func (c *Client) AppLogin(ctx context.Context) (string, error) {
	c.mu.Lock()
	login := c.login
	c.mu.Unlock()
	if login != "" {
		return login, nil
	}
	jwt, err := c.appJWT()
	if err != nil {
		return "", core.Wrap(core.KindInternal, "github.app_login", err)
	}
	var app struct {
		Slug string `json:"slug"`
	}
	if err := c.do(ctx, jwt, http.MethodGet, "/app", nil, &app); err != nil {
		return "", err
	}
	if app.Slug == "" {
		return "", core.Errorf(core.KindTransient, "github.app_login", "the App has no slug")
	}
	c.mu.Lock()
	c.login = app.Slug + "[bot]"
	c.mu.Unlock()
	return app.Slug + "[bot]", nil
}

// appJWT signs a short-lived token identifying the App itself.
func (c *Client) appJWT() (string, error) {
	now := c.now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": c.appID})
	if err != nil {
		return "", err
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", core.Wrap(core.KindInternal, "github.jwt", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Token returns a cached installation token for a repository. Tokens are requested
// only for that repository, so one leaking cannot reach the App's other repositories.
func (c *Client) Token(ctx context.Context, repo string) (core.Secret, time.Time, error) {
	c.mu.Lock()
	t, ok := c.tokens[repo]
	c.mu.Unlock()
	if ok && c.now().Add(tokenSlack).Before(t.expiresAt) {
		return t.value, t.expiresAt, nil
	}
	jwt, err := c.appJWT()
	if err != nil {
		return core.Secret{}, time.Time{}, err
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	if err := c.do(ctx, jwt, http.MethodGet, "/repos/"+repo+"/installation", nil, &inst); err != nil {
		return core.Secret{}, time.Time{}, err
	}
	_, name, _ := strings.Cut(repo, "/")
	var tok struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	body := map[string]any{"repositories": []string{name}}
	if err := c.do(ctx, jwt, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", inst.ID), body, &tok); err != nil {
		return core.Secret{}, time.Time{}, err
	}
	t = installationToken{value: core.NewSecret(tok.Token), expiresAt: tok.ExpiresAt}
	c.mu.Lock()
	c.tokens[repo] = t
	c.mu.Unlock()
	return t.value, t.expiresAt, nil
}

// repoCall performs a request authenticated for one repository.
func (c *Client) repoCall(ctx context.Context, repo, method, path string, in, out any) error {
	tok, _, err := c.Token(ctx, repo)
	if err != nil {
		return err
	}
	return c.do(ctx, tok.Reveal(), method, path, in, out)
}

func (c *Client) do(ctx context.Context, bearer, method, path string, in, out any) error {
	_, err := c.request(ctx, bearer, method, c.api+path, in, out)
	return err
}

var linkNext = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// errListTruncated reports a listing longer than the page bound. Treating what was
// read as complete could lose events and repeat comments, so callers must handle it.
var errListTruncated = core.Errorf(core.KindTransient, "github.list", "the listing has more than %d pages", maxPages)

// errStopList lets a visitor end a listing early, once the rest cannot matter.
var errStopList = errors.New("stop listing")

// list follows pagination up to a bound, appending decoded array items through add.
func (c *Client) list(ctx context.Context, repo, path string, add func(json.RawMessage) error) error {
	return c.pages(ctx, repo, path, func(raw json.RawMessage) error {
		var page []json.RawMessage
		if err := json.Unmarshal(raw, &page); err != nil {
			return core.Wrap(core.KindTransient, "github.list", err)
		}
		for _, item := range page {
			if err := add(item); err != nil {
				return err
			}
		}
		return nil
	})
}

// pages follows pagination up to a bound, handing over each page as it came.
func (c *Client) pages(ctx context.Context, repo, path string, visit func(json.RawMessage) error) error {
	tok, _, err := c.Token(ctx, repo)
	if err != nil {
		return err
	}
	next := c.api + path
	for range maxPages {
		var page json.RawMessage
		hdr, err := c.request(ctx, tok.Reveal(), http.MethodGet, next, nil, &page)
		if err != nil {
			return err
		}
		if err := visit(page); err != nil {
			if errors.Is(err, errStopList) {
				return nil
			}
			return err
		}
		m := linkNext.FindStringSubmatch(hdr.Get("Link"))
		if m == nil {
			return nil
		}
		// Only follow pagination links on the API origin: the bearer token goes with it.
		if !strings.HasPrefix(m[1], c.api+"/") {
			return core.Errorf(core.KindInternal, "github.list", "refusing a pagination link to another origin")
		}
		next = m[1]
	}
	return errListTruncated
}

// graphql runs a query, for what the REST API does not expose. GitHub reports most
// failures here with status 200 and an errors list, so those are checked too.
func (c *Client) graphql(ctx context.Context, repo, query string, vars map[string]any, out any) error {
	tok, _, err := c.Token(ctx, repo)
	if err != nil {
		return err
	}
	var res struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	// api.github.com/graphql, or <host>/api/graphql next to Enterprise's /api/v3.
	target := strings.TrimSuffix(c.api, "/v3") + "/graphql"
	in := map[string]any{"query": query, "variables": vars}
	if _, err := c.request(ctx, tok.Reveal(), http.MethodPost, target, in, &res); err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		e := res.Errors[0]
		kind := core.KindTransient
		switch e.Type {
		case "FORBIDDEN":
			kind = core.KindForbidden
		case "NOT_FOUND":
			kind = core.KindNotFound
		}
		return core.Errorf(kind, "github.graphql", "%s", e.Message)
	}
	if err := json.Unmarshal(res.Data, out); err != nil {
		return core.Wrap(core.KindTransient, "github.graphql", err)
	}
	return nil
}

func (c *Client) request(ctx context.Context, bearer, method, target string, in, out any) (http.Header, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, core.Wrap(core.KindInternal, "github.request", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, core.Wrap(core.KindInternal, "github.request", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "goblins")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, core.Wrap(core.KindCancelled, "github.request", ctx.Err())
		}
		// Transport errors contain the URL but not headers, so no token appears.
		return nil, core.Wrap(core.KindTransient, "github.request", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, core.Wrap(core.KindTransient, "github.request", err)
	}
	if resp.StatusCode >= 300 {
		return resp.Header, apiError(method, target, resp, data)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, core.Wrap(core.KindTransient, "github.request", fmt.Errorf("invalid response: %w", err))
		}
	}
	return resp.Header, nil
}

func apiError(method, target string, resp *http.Response, data []byte) error {
	var msg struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(data, &msg)
	op := "github." + strings.ToLower(method)
	path := target
	if u, err := url.Parse(target); err == nil {
		path = u.Path
	}
	text := fmt.Sprintf("GitHub returned %d for %s %s: %s", resp.StatusCode, method, path, msg.Message)
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return core.Errorf(core.KindUnauthorized, op, "%s", text)
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusForbidden && (resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != ""):
		return core.Errorf(core.KindTransient, op, "%s", text)
	case resp.StatusCode == http.StatusForbidden:
		return core.Errorf(core.KindForbidden, op, "%s. Check the permissions granted to the GitHub App", text)
	case resp.StatusCode == http.StatusNotFound:
		return core.Errorf(core.KindNotFound, op, "%s", text)
	case resp.StatusCode == http.StatusConflict:
		return core.Errorf(core.KindConflict, op, "%s", text)
	case resp.StatusCode == http.StatusUnprocessableEntity:
		return core.Errorf(core.KindInvalid, op, "%s", text)
	case resp.StatusCode >= 500:
		return core.Errorf(core.KindTransient, op, "%s", text)
	}
	return core.Errorf(core.KindInternal, op, "%s", text)
}

// clientFromOptions builds a client from an adapter instance's options.
func clientFromOptions(deps providers.Deps, opts providers.Options) (*Client, error) {
	appID := opts.String("app_id")
	if n, ok := opts["app_id"].(int); ok {
		appID = strconv.Itoa(n)
	}
	key, err := opts.Secret(deps, "private_key")
	if err != nil {
		return nil, err
	}
	if key.Empty() {
		return nil, core.Errorf(core.KindInvalid, "github.client", "private_key_file or private_key_env is required")
	}
	return NewClient(ClientOptions{API: opts.String("api_url"), AppID: appID, PrivateKey: []byte(key.Reveal()), Now: deps.Now})
}

func info(kind core.ProviderKind, name string, caps ...core.Capability) core.ProviderInfo {
	return core.ProviderInfo{Kind: kind, Type: typeName, Name: name, ContractVersion: core.ContractVersion, Capabilities: caps}
}

// Register adds the GitHub adapters to a registry.
func Register(r *providers.Registry) {
	r.Register(providers.KindTracker, typeName, func(name string, deps providers.Deps, opts providers.Options) (any, error) {
		c, err := clientFromOptions(deps, opts)
		if err != nil {
			return nil, err
		}
		return NewTracker(name, c), nil
	})
	r.Register(providers.KindSCM, typeName, func(name string, deps providers.Deps, opts providers.Options) (any, error) {
		c, err := clientFromOptions(deps, opts)
		if err != nil {
			return nil, err
		}
		return NewSCM(name, c, opts.String("git_url")), nil
	})
	r.Register(providers.KindReview, nativeReviewType, func(name string, deps providers.Deps,
		opts providers.Options) (any, error) {
		c, err := clientFromOptions(deps, opts)
		if err != nil {
			return nil, err
		}
		return newNativeReview(name, c, opts)
	})
	r.Register(providers.KindEventSource, typeName, func(name string, deps providers.Deps, opts providers.Options) (any, error) {
		return sourceFromOptions(name, deps, opts)
	})
}

func actorOf(u user) core.Actor {
	a := core.Actor{ID: strconv.FormatInt(u.ID, 10), Login: u.Login, Type: core.ActorUnknown}
	switch {
	case u.Type == "Bot" || strings.HasSuffix(u.Login, "[bot]"):
		a.Type = core.ActorBot
	case u.Type == "User":
		a.Type = core.ActorHuman
	}
	return a
}

type user struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}
