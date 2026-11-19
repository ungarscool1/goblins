// Package kubernetes runs workloads as Kubernetes pods, through the API server's REST
// interface, with no client library. Each workload is one pod with no service account token,
// no privileges and the network policy its spec asks for; durable volumes are
// PersistentVolumeClaims; secrets are Secret objects, never part of the pod spec.
package kubernetes

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// In-cluster service account files, as every pod sees them.
const (
	serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	maxResponse       = 16 << 20
)

// client talks to the API server. The token is read from its file for every request, as
// projected service account tokens are rotated in place.
type client struct {
	base      string
	tokenFile string
	token     core.Secret
	http      *http.Client
	stream    *http.Client // no overall timeout: log streams are long lived
}

// ClientOptions says how to reach the API server. Empty, it uses the pod's own service account.
type ClientOptions struct {
	URL       string
	TokenFile string
	Token     core.Secret
	CAFile    string
	// Getenv finds the in-cluster address.
	Getenv func(string) (string, bool)
}

func newClient(o ClientOptions) (*client, error) {
	if o.URL == "" {
		host, _ := lookup(o.Getenv, "KUBERNETES_SERVICE_HOST")
		port, _ := lookup(o.Getenv, "KUBERNETES_SERVICE_PORT")
		if host == "" || port == "" {
			return nil, core.Errorf(core.KindInvalid, "kubernetes.client",
				"not running in a cluster: set api_url and a token")
		}
		o.URL = "https://" + net.JoinHostPort(host, port)
		if o.TokenFile == "" && o.Token.Empty() {
			o.TokenFile = serviceAccountDir + "/token"
		}
		if o.CAFile == "" {
			o.CAFile = serviceAccountDir + "/ca.crt"
		}
	}
	u, err := url.Parse(o.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !loopback(u.Hostname())) {
		return nil, core.Errorf(core.KindInvalid, "kubernetes.client",
			"api_url must be https (plain http only on loopback, for tests)")
	}
	if o.TokenFile == "" && o.Token.Empty() {
		return nil, core.Errorf(core.KindInvalid, "kubernetes.client", "a token is required")
	}
	tlsConf := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, core.Errorf(core.KindInvalid, "kubernetes.client",
				"cannot read the CA file")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, core.Errorf(core.KindInvalid, "kubernetes.client",
				"the CA file holds no certificate")
		}
		tlsConf.RootCAs = pool
	}
	tr := &http.Transport{TLSClientConfig: tlsConf, MaxIdleConns: 8,
		IdleConnTimeout: 30 * time.Second}
	return &client{base: strings.TrimRight(u.String(), "/"), tokenFile: o.TokenFile, token: o.Token,
		http:   &http.Client{Transport: tr, Timeout: 60 * time.Second},
		stream: &http.Client{Transport: tr}}, nil
}

func lookup(env func(string) (string, bool), k string) (string, bool) {
	if env == nil {
		return os.LookupEnv(k)
	}
	return env(k)
}

func loopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func (c *client) bearer() (string, error) {
	if c.tokenFile == "" {
		return c.token.Reveal(), nil
	}
	b, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return "", core.Errorf(core.KindAuthRequired, "kubernetes.client",
			"cannot read the token file")
	}
	return strings.TrimSpace(string(b)), nil
}

func (c *client) request(ctx context.Context, method, path, contentType string,
	in any) (*http.Request, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, core.Wrap(core.KindInternal, "kubernetes.call", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, core.Wrap(core.KindInternal, "kubernetes.call", err)
	}
	tok, err := c.bearer()
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", contentType)
	}
	return req, nil
}

// call sends one request and decodes the answer into out.
func (c *client) call(ctx context.Context, method, path string, in, out any) error {
	return c.send(ctx, method, path, "application/json", in, out)
}

// patch merges a JSON patch into an object.
func (c *client) patch(ctx context.Context, path string, in any) error {
	return c.send(ctx, http.MethodPatch, path, "application/merge-patch+json", in, nil)
}

func (c *client) send(ctx context.Context, method, path, contentType string, in, out any) error {
	req, err := c.request(ctx, method, path, contentType, in)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return core.Wrap(core.KindCancelled, "kubernetes.call", ctx.Err())
		}
		return core.Wrap(core.KindTransient, "kubernetes.call",
			errors.New("the API server cannot be reached"))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return core.Wrap(core.KindTransient, "kubernetes.call", err)
	}
	if resp.StatusCode/100 != 2 {
		return classify(method+" "+path, resp.StatusCode, data)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return core.Wrap(core.KindTransient, "kubernetes.call",
				fmt.Errorf("unreadable answer: %w", err))
		}
	}
	return nil
}

// classify maps the API server's Status answers onto error kinds. Its message is kept: it
// names objects and fields, never secret values.
func classify(op string, code int, data []byte) error {
	var st struct {
		Message string `json:"message"`
		Reason  string `json:"reason"`
	}
	_ = json.Unmarshal(data, &st)
	msg := st.Message
	if msg == "" {
		msg = http.StatusText(code)
	}
	kind := core.KindInternal
	switch {
	case code == http.StatusNotFound:
		kind = core.KindNotFound
	case code == http.StatusConflict:
		kind = core.KindConflict
	case code == http.StatusUnauthorized:
		kind = core.KindAuthRequired
	case code == http.StatusForbidden:
		kind = core.KindForbidden
		msg += ". Check the role bound to Goblins' service account"
	case code == http.StatusUnprocessableEntity, code == http.StatusBadRequest:
		kind = core.KindInvalid
	case code == http.StatusTooManyRequests, code >= 500:
		kind = core.KindTransient
	}
	return core.Errorf(kind, "kubernetes", "%s: %s", op, msg)
}
