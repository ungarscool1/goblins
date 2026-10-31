package fakegithub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type gitRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// PullRequest mirrors the REST representation.
type PullRequest struct {
	Number    int64     `json:"number"`
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	Draft     bool      `json:"draft"`
	User      User      `json:"user"`
	Head      gitRef    `json:"head"`
	Base      gitRef    `json:"base"`
	HTMLURL   string    `json:"html_url"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SetBranch sets a branch head, as a push would.
func (s *Server) SetBranch(repo, branch, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repo(repo).branches[branch] = sha
}

// PullRequests lists all pull requests.
func (s *Server) PullRequests(repo string) []PullRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.repo(repo)
	out := []PullRequest{}
	for _, n := range sortedKeys(st.pulls) {
		out = append(out, *st.pulls[n])
	}
	return out
}

// Reviews lists the reviews posted on a pull request.
func (s *Server) Reviews(repo string, n int64) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any{}, s.repo(repo).reviews[n]...)
}

// SetCheckRun adds a CI result for a commit.
func (s *Server) SetCheckRun(repo, sha, name, status, conclusion string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.repo(repo)
	st.checks[sha] = append(st.checks[sha], map[string]any{"name": name, "status": status, "conclusion": conclusion, "html_url": "https://ci.example/" + name})
}

func (s *Server) branch(repo string, st *repoState, name string) (string, bool) {
	if s.BranchLookup != nil {
		return s.BranchLookup(repo, name)
	}
	sha, ok := st.branches[name]
	return sha, ok
}

func (s *Server) routeSCM(w http.ResponseWriter, r *http.Request, repo string, st *repoState, seg []string) bool {
	switch {
	case seg[0] == "branches" && len(seg) == 2 && r.Method == http.MethodGet:
		sha, ok := s.branch(repo, st, seg[1])
		if !ok {
			fail(w, 404, "Branch not found")
			return true
		}
		writeJSON(w, 200, map[string]any{"name": seg[1], "commit": map[string]string{"sha": sha}})
	case seg[0] == "commits" && len(seg) == 2:
		writeJSON(w, 200, map[string]any{"sha": seg[1], "commit": map[string]any{"message": "commit " + seg[1], "author": map[string]string{"name": "someone"}}})
	case seg[0] == "commits" && len(seg) == 3 && seg[2] == "check-runs":
		runs := st.checks[seg[1]]
		if runs == nil {
			runs = []map[string]any{}
		}
		writeJSON(w, 200, map[string]any{"total_count": len(runs), "check_runs": runs})
	case seg[0] == "pulls":
		s.routePulls(w, r, repo, st, seg)
	default:
		return false
	}
	return true
}

func (s *Server) routePulls(w http.ResponseWriter, r *http.Request, repo string, st *repoState, seg []string) {
	if s.DenyPullRequests && r.Method != http.MethodGet {
		fail(w, 403, "Resource not accessible by integration")
		return
	}
	now := s.Now().UTC().Truncate(time.Second)
	switch {
	case len(seg) == 1 && r.Method == http.MethodGet:
		out := []*PullRequest{}
		head := r.URL.Query().Get("head")
		state := r.URL.Query().Get("state")
		for _, n := range sortedKeys(st.pulls) {
			p := st.pulls[n]
			if state != "all" && p.State != "open" {
				continue
			}
			if head != "" && !strings.HasSuffix(head, ":"+p.Head.Ref) {
				continue
			}
			out = append(out, p)
		}
		writeJSON(w, 200, out)
	case len(seg) == 1 && r.Method == http.MethodPost:
		var b struct {
			Title string `json:"title"`
			Body  string `json:"body"`
			Head  string `json:"head"`
			Base  string `json:"base"`
			Draft bool   `json:"draft"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		sha, ok := s.branch(repo, st, b.Head)
		if !ok {
			fail(w, 422, "Validation Failed: head does not exist")
			return
		}
		baseSHA, ok := s.branch(repo, st, b.Base)
		if !ok {
			fail(w, 422, "Validation Failed: base does not exist")
			return
		}
		for _, p := range st.pulls {
			if p.Head.Ref == b.Head && p.State == "open" {
				fail(w, 422, "A pull request already exists")
				return
			}
		}
		st.nextNum++
		p := &PullRequest{Number: st.nextNum, ID: s.nextID(), Title: b.Title, Body: b.Body, State: "open", Draft: b.Draft, User: BotUser, Head: gitRef{Ref: b.Head, SHA: sha}, Base: gitRef{Ref: b.Base, SHA: baseSHA},
			HTMLURL: fmt.Sprintf("https://github.example/%s/pull/%d", repo, st.nextNum), UpdatedAt: now}
		st.pulls[p.Number] = p
		writeJSON(w, 201, p)
	case len(seg) >= 2:
		n, err := strconv.ParseInt(seg[1], 10, 64)
		p, ok := st.pulls[n]
		if err != nil || !ok {
			fail(w, 404, "no pull request")
			return
		}
		p.Head.SHA, _ = s.branch(repo, st, p.Head.Ref)
		switch {
		case len(seg) == 2 && r.Method == http.MethodGet:
			writeJSON(w, 200, p)
		case len(seg) == 2 && r.Method == http.MethodPatch:
			var b struct {
				Title *string `json:"title"`
				Body  *string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b.Title != nil {
				p.Title = *b.Title
			}
			if b.Body != nil {
				p.Body = *b.Body
			}
			p.UpdatedAt = now
			writeJSON(w, 200, p)
		case len(seg) == 3 && seg[2] == "reviews" && r.Method == http.MethodPost:
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			st.reviews[n] = append(st.reviews[n], b)
			writeJSON(w, 200, map[string]any{"id": s.nextID()})
		case len(seg) == 3 && seg[2] == "reviews" && r.Method == http.MethodGet:
			out := []map[string]any{}
			for i, rev := range st.reviews[n] {
				out = append(out, map[string]any{"id": i + 1, "body": rev["body"], "user": BotUser, "state": "COMMENTED"})
			}
			writeJSON(w, 200, out)
		case len(seg) == 3 && seg[2] == "comments" && r.Method == http.MethodGet:
			out := []map[string]any{}
			for _, rev := range st.reviews[n] {
				if cs, ok := rev["comments"].([]any); ok {
					for i, c := range cs {
						m, _ := c.(map[string]any)
						out = append(out, map[string]any{"id": i + 1, "path": m["path"], "line": m["line"], "body": m["body"], "user": BotUser})
					}
				}
			}
			writeJSON(w, 200, out)
		default:
			fail(w, 404, "unknown endpoint")
		}
	default:
		fail(w, 404, "unknown endpoint")
	}
}
