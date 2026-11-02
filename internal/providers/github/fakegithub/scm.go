package fakegithub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
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
	Merged    bool      `json:"merged"`
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
	case seg[0] == "branches" && len(seg) == 3 && seg[2] == "protection" && r.Method == http.MethodGet:
		if s.ProtectionDenied {
			fail(w, 403, "Resource not accessible by integration")
			return true
		}
		p, ok := st.protection[seg[1]]
		if !ok {
			fail(w, 404, "Branch not protected")
			return true
		}
		writeJSON(w, 200, p)
	case seg[0] == "commits" && len(seg) == 2:
		writeJSON(w, 200, map[string]any{"sha": seg[1], "commit": map[string]any{"message": "commit " + seg[1], "author": map[string]string{"name": "someone"}}})
	case seg[0] == "commits" && len(seg) == 3 && seg[2] == "check-runs":
		runs := st.checks[seg[1]]
		perPage, page := 30, 1
		if v, err := strconv.Atoi(r.URL.Query().Get("per_page")); err == nil && v > 0 {
			perPage = min(v, 100)
		}
		if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && v > 0 {
			page = v
		}
		from, to := min((page-1)*perPage, len(runs)), min(page*perPage, len(runs))
		if to < len(runs) {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?per_page=%d&page=%d>; rel="next"`, r.Host, r.URL.Path, perPage, page+1))
		}
		writeJSON(w, 200, map[string]any{"total_count": len(runs), "check_runs": append([]map[string]any{}, runs[from:to]...)})
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
				author, ok := rev["user"]
				if !ok {
					author = BotUser
				}
				state, ok := rev["state"]
				if !ok {
					state = "COMMENTED"
				}
				out = append(out, map[string]any{"id": i + 1, "body": rev["body"], "user": author,
					"state": state, "commit_id": rev["commit_id"]})
			}
			writeJSON(w, 200, out)
		case len(seg) == 3 && seg[2] == "files" && r.Method == http.MethodGet:
			out := []map[string]any{}
			for _, f := range st.files[n] {
				out = append(out, map[string]any{"filename": f})
			}
			writeJSON(w, 200, out)
		case len(seg) == 3 && seg[2] == "merge" && r.Method == http.MethodPut:
			var b struct {
				SHA string `json:"sha"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b.SHA != p.Head.SHA {
				fail(w, 409, "Head branch was modified")
				return
			}
			s.Merges = append(s.Merges, b.SHA)
			p.State, p.Merged, p.UpdatedAt = "closed", true, now
			writeJSON(w, 200, map[string]any{"merged": true})
		case len(seg) == 3 && seg[2] == "comments" && r.Method == http.MethodGet:
			writeJSON(w, 200, append(reviewComments(st.reviews[n]), st.replies[n]...))
		default:
			fail(w, 404, "unknown endpoint")
		}
	default:
		fail(w, 404, "unknown endpoint")
	}
}

// reviewCommentID gives each inline comment an ID of its own, as GitHub does.
func reviewCommentID(review, comment int) int { return review*1000 + comment + 1 }

func reviewComments(reviews []map[string]any) []map[string]any {
	out := []map[string]any{}
	for r, rev := range reviews {
		comments, ok := rev["comments"].([]any)
		if !ok {
			continue
		}
		for i, comment := range comments {
			m, _ := comment.(map[string]any)
			author, ok := rev["user"]
			if !ok {
				author = BotUser
			}
			out = append(out, map[string]any{
				"id":   reviewCommentID(r, i),
				"path": m["path"],
				"line": m["line"],
				"body": m["body"],
				"user": author,

				"commit_id":              rev["commit_id"],
				"created_at":             rev["submitted_at"],
				"pull_request_review_id": r + 1,
			})
		}
	}
	return out
}

// AddReview submits a review as someone else, with inline comments of the form path, line, body.
func (s *Server) AddReview(repo string, n int64, author User, commit, state, body string,
	inline ...map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.repo(repo)
	comments := make([]any, len(inline))
	for i, c := range inline {
		comments[i] = c
	}
	st.reviews[n] = append(st.reviews[n], map[string]any{"user": author, "commit_id": commit,
		"state": state, "body": body, "comments": comments, "submitted_at": s.Now().UTC()})
}

// SetFiles seeds what a pull request changes.
func (s *Server) SetFiles(repo string, n int64, files ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repo(repo).files[n] = files
}

// SetProtection seeds a branch's protection, in the REST shape.
func (s *Server) SetProtection(repo, branch string, p map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repo(repo).protection[branch] = p
}

// ClosePullRequest closes or merges a pull request as a person would.
func (s *Server) ClosePullRequest(repo string, n int64, merged bool) PullRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.repo(repo).pulls[n]
	p.State, p.Merged, p.UpdatedAt = "closed", merged, s.Now().UTC().Truncate(time.Second)
	return *p
}

// AddReviewReply answers a review comment, as a person would on GitHub.
func (s *Server) AddReviewReply(repo string, n, id, replyTo int64, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.repo(repo)
	if st.replies == nil {
		st.replies = map[int64][]map[string]any{}
	}
	st.replies[n] = append(st.replies[n], map[string]any{"id": id, "in_reply_to_id": replyTo, "body": body, "user": BotUser})
}

// ResolveThread marks the review thread started by a comment as resolved.
func (s *Server) ResolveThread(repo string, n, commentID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.repo(repo)
	if st.resolved == nil {
		st.resolved = map[string]bool{}
	}
	st.resolved[fmt.Sprintf("%d/%d", n, commentID)] = true
}

// serveGraphQL answers the one query Goblins sends, one thread per page so that a test
// exercises pagination. A query not asking for the fields it reads is refused, as the
// real schema would give it nothing.
func (s *Server) serveGraphQL(w http.ResponseWriter, r *http.Request, bearer string) {
	var req struct {
		Query     string `json:"query"`
		Variables struct {
			Owner  string `json:"owner"`
			Name   string `json:"name"`
			Number int64  `json:"number"`
			After  string `json:"after"`
		} `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, "bad request")
		return
	}
	info, ok := s.tokens[bearer]
	if !ok || s.Now().After(info.expires) {
		fail(w, 401, "bad credentials")
		return
	}
	v := req.Variables
	st, ok := s.repos[v.Owner+"/"+v.Name]
	if !ok || !slices.Contains(info.repos, v.Name) {
		writeJSON(w, 200, map[string]any{"errors": []map[string]string{{"type": "NOT_FOUND", "message": "Could not resolve to a Repository"}}})
		return
	}
	if !strings.Contains(req.Query, "isResolved") || !strings.Contains(req.Query, "fullDatabaseId") {
		writeJSON(w, 200, map[string]any{"errors": []map[string]string{{"message": "unexpected query"}}})
		return
	}
	var ids []int
	for r, rev := range st.reviews[v.Number] {
		if cs, ok := rev["comments"].([]any); ok {
			for i := range cs {
				ids = append(ids, reviewCommentID(r, i))
			}
		}
	}
	at, _ := strconv.Atoi(v.After)
	threads := map[string]any{"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}, "nodes": []any{}}
	if at < len(ids) {
		threads["pageInfo"] = map[string]any{"hasNextPage": at+1 < len(ids), "endCursor": strconv.Itoa(at + 1)}
		threads["nodes"] = []any{map[string]any{
			"isResolved": st.resolved[fmt.Sprintf("%d/%d", v.Number, ids[at])],
			"comments":   map[string]any{"nodes": []any{map[string]string{"fullDatabaseId": strconv.Itoa(ids[at])}}},
		}}
	}
	writeJSON(w, 200, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": threads}}}})
}
