// Package fakegithub is an in-memory GitHub REST API for tests and offline
// end-to-end runs. It enforces App authentication like the real service, so tests
// exercise the adapter's actual token flow without any network or credentials.
package fakegithub

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Installation is the only installation of the fake App.
const Installation = 77

// User is a GitHub account.
type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

// BotUser is the fake App's identity.
var BotUser = User{ID: 9000, Login: "goblins-app[bot]", Type: "Bot"}

// Issue mirrors the REST representation.
type Issue struct {
	Number    int64     `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	User      User      `json:"user"`
	Labels    []Label   `json:"labels"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Label mirrors the REST representation.
type Label struct {
	Name string `json:"name"`
}

// Comment mirrors the REST representation.
type Comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      User      `json:"user"`
	IssueURL  string    `json:"issue_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type issueEvent struct {
	Event     string    `json:"event"`
	Actor     User      `json:"actor"`
	Label     Label     `json:"label"`
	Issue     *Issue    `json:"issue"`
	CreatedAt time.Time `json:"created_at"`
}

// Server is the fake API.
type Server struct {
	*httptest.Server
	Key *rsa.PrivateKey
	Now func() time.Time

	mu       sync.Mutex
	repos    map[string]*repoState
	tokens   map[string]tokenInfo
	seq      int64
	Requests []string
	// TokenRepos records the repositories of each token request.
	TokenRepos [][]string
	// FailNext makes the next matching request return this status once.
	failNext map[string]int
	// DenyPullRequests makes mutating calls to pull requests return 403.
	DenyPullRequests bool
	// BranchLookup, when set, answers branch heads, so a real Git remote can back them.
	BranchLookup func(repo, branch string) (string, bool)
}

type tokenInfo struct {
	repos   []string
	expires time.Time
}

type repoState struct {
	issues   map[int64]*Issue
	comments map[int64][]*Comment
	events   []issueEvent
	perms    map[string]string
	pulls    map[int64]*PullRequest
	reviews  map[int64][]map[string]any
	branches map[string]string
	checks   map[string][]map[string]any
	nextNum  int64
}

// New starts a fake with a fresh key. It is closed when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Key: key, Now: time.Now, repos: map[string]*repoState{}, tokens: map[string]tokenInfo{}, failNext: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// KeyPEM returns the App private key.
func (s *Server) KeyPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(s.Key)})
}

func (s *Server) repo(name string) *repoState {
	r, ok := s.repos[name]
	if !ok {
		r = &repoState{issues: map[int64]*Issue{}, comments: map[int64][]*Comment{}, perms: map[string]string{}, pulls: map[int64]*PullRequest{},
			reviews: map[int64][]map[string]any{}, branches: map[string]string{}, checks: map[string][]map[string]any{}}
		s.repos[name] = r
	}
	return r
}

// AddRepo makes a repository visible to the App.
func (s *Server) AddRepo(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repo(name)
}

// Human builds a user account.
func Human(id int64, login string) User { return User{ID: id, Login: login, Type: "User"} }

// Grant sets a collaborator's role.
func (s *Server) Grant(repo, login, role string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repo(repo).perms[login] = role
}

// FailOnce makes the next request whose "METHOD path" has this prefix fail.
func (s *Server) FailOnce(prefix string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext[prefix] = status
}

func (s *Server) nextID() int64 { s.seq++; return s.seq + 1000 }

// AddIssue creates an issue.
func (s *Server) AddIssue(repo, title, body string, author User) Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.repo(repo)
	r.nextNum++
	now := s.Now().UTC().Truncate(time.Second)
	i := &Issue{Number: r.nextNum, Title: title, Body: body, State: "open", User: author, Labels: []Label{}, CreatedAt: now, UpdatedAt: now,
		HTMLURL: fmt.Sprintf("https://github.example/%s/issues/%d", repo, r.nextNum)}
	r.issues[i.Number] = i
	return *i
}

// EditIssue changes an issue's body.
func (s *Server) EditIssue(repo string, n int64, body string) Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.repo(repo).issues[n]
	i.Body = body
	i.UpdatedAt = s.Now().UTC().Truncate(time.Second)
	return *i
}

// AddLabel applies a label as an actor.
func (s *Server) AddLabel(repo string, n int64, label string, actor User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.repo(repo)
	i := r.issues[n]
	i.Labels = append(i.Labels, Label{Name: label})
	now := s.Now().UTC().Truncate(time.Second)
	i.UpdatedAt = now
	cp := *i
	r.events = append(r.events, issueEvent{Event: "labeled", Actor: actor, Label: Label{Name: label}, Issue: &cp, CreatedAt: now})
}

// AddComment posts a comment as an actor.
func (s *Server) AddComment(repo string, n int64, author User, body string) Comment {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.addComment(repo, n, author, body)
}

func (s *Server) addComment(repo string, n int64, author User, body string) *Comment {
	now := s.Now().UTC().Truncate(time.Second)
	c := &Comment{ID: s.nextID(), Body: body, User: author, IssueURL: fmt.Sprintf("%s/repos/%s/issues/%d", s.URL, repo, n), CreatedAt: now, UpdatedAt: now}
	r := s.repo(repo)
	r.comments[n] = append(r.comments[n], c)
	return c
}

// Comments returns an issue's conversation.
func (s *Server) Comments(repo string, n int64) []Comment {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Comment{}
	for _, c := range s.repo(repo).comments[n] {
		out = append(out, *c)
	}
	return out
}

// GetIssue returns an issue.
func (s *Server) GetIssue(repo string, n int64) Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.repo(repo).issues[n]
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"message": msg})
}

func (s *Server) verifyJWT(tok string) bool {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&s.Key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	return json.Unmarshal(raw, &claims) == nil && s.Now().Unix() < claims.Exp
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reqLine := r.Method + " " + r.URL.Path
	s.Requests = append(s.Requests, reqLine)
	for prefix, status := range s.failNext {
		if strings.HasPrefix(reqLine, prefix) {
			delete(s.failNext, prefix)
			fail(w, status, "injected failure")
			return
		}
	}
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	path := strings.Trim(r.URL.Path, "/")
	seg := strings.Split(path, "/")

	// App-level endpoints authenticate with the App JWT.
	if len(seg) == 5 && seg[0] == "repos" && seg[3] == "installation" || strings.HasPrefix(path, "app/") {
		if !s.verifyJWT(bearer) {
			fail(w, 401, "bad JWT")
			return
		}
		if strings.HasPrefix(path, "app/") {
			var body struct {
				Repositories []string `json:"repositories"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.TokenRepos = append(s.TokenRepos, body.Repositories)
			tok := "ghs_fake" + strconv.FormatInt(s.nextID(), 10)
			exp := s.Now().Add(time.Hour)
			s.tokens[tok] = tokenInfo{repos: body.Repositories, expires: exp}
			writeJSON(w, 201, map[string]any{"token": tok, "expires_at": exp.UTC().Format(time.RFC3339)})
			return
		}
		if _, ok := s.repos[seg[1]+"/"+seg[2]]; !ok {
			fail(w, 404, "not installed")
			return
		}
		writeJSON(w, 200, map[string]int{"id": Installation})
		return
	}
	if len(seg) == 4 && seg[0] == "repos" && seg[3] == "installation" {
		if !s.verifyJWT(bearer) {
			fail(w, 401, "bad JWT")
			return
		}
		if _, ok := s.repos[seg[1]+"/"+seg[2]]; !ok {
			fail(w, 404, "not installed")
			return
		}
		writeJSON(w, 200, map[string]int{"id": Installation})
		return
	}
	if len(seg) < 4 || seg[0] != "repos" {
		fail(w, 404, "unknown endpoint")
		return
	}
	repo := seg[1] + "/" + seg[2]
	info, ok := s.tokens[bearer]
	if !ok || s.Now().After(info.expires) {
		fail(w, 401, "bad credentials")
		return
	}
	if !slices.Contains(info.repos, seg[2]) {
		fail(w, 403, "token is not scoped to this repository")
		return
	}
	st, ok := s.repos[repo]
	if !ok {
		fail(w, 404, "no repository")
		return
	}
	s.route(w, r, repo, st, seg[3:])
}

func since(r *http.Request) time.Time {
	t, _ := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
	return t
}

func (s *Server) route(w http.ResponseWriter, r *http.Request, repo string, st *repoState, seg []string) {
	switch {
	case seg[0] == "collaborators" && len(seg) == 3 && seg[2] == "permission":
		role, ok := st.perms[seg[1]]
		if !ok {
			fail(w, 404, "not a collaborator")
			return
		}
		perm := role
		switch role {
		case "maintain":
			perm = "write"
		case "triage":
			perm = "read"
		}
		writeJSON(w, 200, map[string]string{"permission": perm, "role_name": role})
	case seg[0] == "issues":
		s.routeIssues(w, r, repo, st, seg)
	case s.routeSCM(w, r, repo, st, seg):
	default:
		fail(w, 404, "unknown endpoint")
	}
}

func (s *Server) routeIssues(w http.ResponseWriter, r *http.Request, repo string, st *repoState, seg []string) {
	switch {
	case len(seg) == 1 && r.Method == http.MethodGet:
		out := []map[string]any{}
		state := r.URL.Query().Get("state")
		for _, n := range sortedKeys(st.issues) {
			i := st.issues[n]
			if i.UpdatedAt.Before(since(r)) || (state == "open" && i.State != "open") {
				continue
			}
			out = append(out, asMap(i))
		}
		for _, n := range sortedKeys(st.pulls) {
			p := st.pulls[n]
			if !p.UpdatedAt.Before(since(r)) {
				// The real issues endpoint returns pull requests too.
				out = append(out, map[string]any{"number": p.Number, "title": p.Title, "state": p.State, "user": p.User, "pull_request": map[string]string{"url": "x"}, "updated_at": p.UpdatedAt, "created_at": p.UpdatedAt})
			}
		}
		writeJSON(w, 200, out)
	case len(seg) == 2 && seg[1] == "comments" && r.Method == http.MethodGet:
		out := []*Comment{}
		for _, n := range sortedKeys(st.comments) {
			for _, c := range st.comments[n] {
				if !c.UpdatedAt.Before(since(r)) {
					out = append(out, c)
				}
			}
		}
		writeJSON(w, 200, out)
	case len(seg) == 2 && seg[1] == "events" && r.Method == http.MethodGet:
		writeJSON(w, 200, st.events)
	case len(seg) >= 2:
		n, err := strconv.ParseInt(seg[1], 10, 64)
		if err != nil {
			fail(w, 404, "bad issue")
			return
		}
		i, ok := st.issues[n]
		if !ok {
			if _, isPR := st.pulls[n]; !isPR || len(seg) != 3 || seg[2] != "comments" {
				fail(w, 404, "no issue")
				return
			}
		}
		s.routeIssue(w, r, repo, st, i, n, seg[2:])
	default:
		fail(w, 404, "unknown endpoint")
	}
}

func (s *Server) routeIssue(w http.ResponseWriter, r *http.Request, repo string, st *repoState, i *Issue, n int64, seg []string) {
	switch {
	case len(seg) == 0 && r.Method == http.MethodGet:
		writeJSON(w, 200, i)
	case len(seg) == 0 && r.Method == http.MethodPatch:
		var body struct {
			State string `json:"state"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		i.State = body.State
		writeJSON(w, 200, i)
	case seg[0] == "comments" && r.Method == http.MethodGet:
		out := []*Comment{}
		for _, c := range st.comments[n] {
			if !c.UpdatedAt.Before(since(r)) {
				out = append(out, c)
			}
		}
		writeJSON(w, 200, out)
	case seg[0] == "comments" && r.Method == http.MethodPost:
		var body struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeJSON(w, 201, s.addComment(repo, n, BotUser, body.Body))
	case seg[0] == "labels" && r.Method == http.MethodPost:
		var body struct {
			Labels []string `json:"labels"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, l := range body.Labels {
			i.Labels = append(i.Labels, Label{Name: l})
		}
		writeJSON(w, 200, i.Labels)
	case seg[0] == "labels" && r.Method == http.MethodDelete && len(seg) == 2:
		i.Labels = slices.DeleteFunc(i.Labels, func(l Label) bool { return l.Name == seg[1] })
		writeJSON(w, 200, i.Labels)
	default:
		fail(w, 404, "unknown endpoint")
	}
}

func sortedKeys[V any](m map[int64]V) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func asMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// Sign returns the signature header for a webhook body.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
