package console

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// Operator is someone allowed into the console.
type Operator struct {
	Name string
	// CanAct is false for a viewer.
	CanAct bool
	Token  core.Secret
}

// MinTokenLength keeps tokens out of reach of guessing.
const MinTokenLength = 32

const (
	cookieName = "goblins_console"
	maxForm    = 64 << 10
)

type operatorKey struct{}

func operatorOf(r *http.Request) *Operator {
	op, _ := r.Context().Value(operatorKey{}).(*Operator)
	return op
}

// match finds the operator a token belongs to. Every token is compared, in constant time, so
// timing tells nothing about which one came close.
func (c *console) match(token string) *Operator {
	if token == "" {
		return nil
	}
	got := sha256.Sum256([]byte(token))
	var found *Operator
	for i := range c.o.Operators {
		want := sha256.Sum256([]byte(c.o.Operators[i].Token.Reveal()))
		if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
			found = &c.o.Operators[i]
		}
	}
	return found
}

// session is one browser's login. It lives in memory only: a restart logs everyone out, and
// nothing about it can be read back from disk.
type session struct {
	op      *Operator
	csrf    string
	expires time.Time
}

type sessions struct {
	mu  sync.Mutex
	m   map[string]session
	ttl time.Duration
	now func() time.Time
}

func newSessions(ttl time.Duration, now func() time.Time) *sessions {
	return &sessions{m: map[string]session{}, ttl: ttl, now: now}
}

func randomString() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *sessions) start(op *Operator) (string, session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for id, ses := range s.m {
		if !now.Before(ses.expires) {
			delete(s.m, id)
		}
	}
	id := randomString()
	ses := session{op: op, csrf: randomString(), expires: now.Add(s.ttl)}
	s.m[id] = ses
	return id, ses
}

func (s *sessions) get(id string) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ses, ok := s.m[id]
	if !ok || !s.now().Before(ses.expires) {
		delete(s.m, id)
		return session{}, false
	}
	return ses, true
}

func (s *sessions) end(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

// throttle slows down guessing: after ten failed logins in a minute, logins wait.
type throttle struct {
	mu       sync.Mutex
	failures []time.Time
	now      func() time.Time
}

func (t *throttle) recent() []time.Time {
	cut := t.now().Add(-time.Minute)
	kept := t.failures[:0]
	for _, f := range t.failures {
		if f.After(cut) {
			kept = append(kept, f)
		}
	}
	t.failures = kept
	return kept
}

func (t *throttle) blocked() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.recent()) >= 10
}

func (t *throttle) fail() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failures = append(t.recent(), t.now())
}

type loginView struct{ Failed bool }

func (c *console) loginPage(w http.ResponseWriter, r *http.Request) {
	c.render(w, "login.html", loginView{Failed: r.URL.Query().Has("failed")})
}

func (c *console) login(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	if c.throttle.blocked() {
		http.Error(w, "too many failed logins, wait a minute", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	op := c.match(r.PostFormValue("token"))
	if op == nil {
		c.throttle.fail()
		c.o.Log.Warn("console login refused", "remote", r.RemoteAddr)
		http.Redirect(w, r, "/login?failed", http.StatusSeeOther)
		return
	}
	id, _ := c.sessions.start(op)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: id, Path: "/", HttpOnly: true,
		Secure: !c.o.InsecureCookie, SameSite: http.SameSiteStrictMode,
		MaxAge: int(c.o.SessionTTL / time.Second)})
	c.o.Log.Info("console login", "operator", op.Name, "remote", r.RemoteAddr)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (c *console) logout(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie(cookieName); err == nil && sameOrigin(r) {
		if ses, ok := c.sessions.get(ck.Value); ok &&
			subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(ses.csrf)) == 1 {
			c.sessions.end(ck.Value)
		}
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: !c.o.InsecureCookie, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type sessionKey struct{}

func sessionOf(r *http.Request) session {
	ses, _ := r.Context().Value(sessionKey{}).(session)
	return ses
}

// page serves a page to a logged-in browser and sends anyone else to the login page.
func (c *console) page(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ck, err := r.Cookie(cookieName)
		var ses session
		ok := err == nil
		if ok {
			ses, ok = c.sessions.get(ck.Value)
		}
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		ctx := context.WithValue(r.Context(), operatorKey{}, ses.op)
		h(w, r.WithContext(context.WithValue(ctx, sessionKey{}, ses)))
	})
}

// form takes an action from a page. A browser sends the session cookie with any request, so
// the request must also come from this site and carry the session's own form token.
func (c *console) form(h http.HandlerFunc) http.Handler {
	return c.page(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxForm)
		ses := sessionOf(r)
		if !sameOrigin(r) ||
			subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(ses.csrf)) != 1 {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		h(w, r)
	})
}

// api serves the JSON API to a bearer token. Browsers never send one on their own, so it
// needs no form token.
func (c *console) api(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		op := c.match(strings.TrimSpace(token))
		if !ok || op == nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="goblins"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxForm)
		h(w, r.WithContext(context.WithValue(r.Context(), operatorKey{}, op)))
	})
}

// sameOrigin refuses requests a browser marks as coming from another site. Clients that are
// not browsers send neither header.
func sameOrigin(r *http.Request) bool {
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" && s != "same-origin" && s != "none" {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		return err == nil && u.Host == r.Host
	}
	return true
}
