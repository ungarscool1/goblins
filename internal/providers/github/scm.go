package github

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ungarscool1/goblins/internal/core"
)

const defaultGitBase = "https://github.com"

// SCM implements core.SourceControl on GitHub repositories.
type SCM struct {
	name    string
	c       *Client
	gitBase string
	tracker *Tracker
}

// NewSCM returns an SCM using c. gitBase is the Git transport root, empty for GitHub.
func NewSCM(name string, c *Client, gitBase string) *SCM {
	if gitBase == "" {
		gitBase = defaultGitBase
	}
	return &SCM{name: name, c: c, gitBase: strings.TrimRight(gitBase, "/"), tracker: NewTracker(name, c)}
}

// Info implements core.Provider.
func (s *SCM) Info() core.ProviderInfo {
	return info(core.ProviderSCM, s.name, core.CapSCMPullRequests, core.CapSCMDraft, core.CapSCMInlineReview, core.CapSCMStatusChecks)
}

// GitAccess implements core.SourceControl. The token is scoped to this repository and
// short lived. It is carried apart from the URL, so it cannot leak through one.
func (s *SCM) GitAccess(ctx context.Context, repo core.RepoRef, _ bool) (core.GitAccess, error) {
	tok, exp, err := s.c.Token(ctx, repo.Slug)
	if err != nil {
		return core.GitAccess{}, err
	}
	return core.GitAccess{RemoteURL: s.gitBase + "/" + repo.Slug + ".git", Username: "x-access-token", Password: tok, ExpiresAt: exp}, nil
}

// ResolveBranch implements core.SourceControl.
func (s *SCM) ResolveBranch(ctx context.Context, repo core.RepoRef, branch string) (string, error) {
	var res struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := s.c.repoCall(ctx, repo.Slug, http.MethodGet, "/repos/"+repo.Slug+"/branches/"+url.PathEscape(branch), nil, &res); err != nil {
		return "", err
	}
	return res.Commit.SHA, nil
}

// GetCommit implements core.SourceControl.
func (s *SCM) GetCommit(ctx context.Context, repo core.RepoRef, sha string) (core.Commit, error) {
	var res struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name string `json:"name"`
			} `json:"author"`
		} `json:"commit"`
	}
	if err := s.c.repoCall(ctx, repo.Slug, http.MethodGet, "/repos/"+repo.Slug+"/commits/"+url.PathEscape(sha), nil, &res); err != nil {
		return core.Commit{}, err
	}
	return core.Commit{SHA: res.SHA, Message: res.Commit.Message, Author: res.Commit.Author.Name}, nil
}

func (s *SCM) findOpen(ctx context.Context, repo core.RepoRef, head string) (*pullRequest, error) {
	owner, _, _ := strings.Cut(repo.Slug, "/")
	q := url.Values{"state": {"open"}, "head": {owner + ":" + head}, "per_page": {"100"}}
	var found *pullRequest
	err := s.c.list(ctx, repo.Slug, "/repos/"+repo.Slug+"/pulls?"+q.Encode(), func(raw json.RawMessage) error {
		var p pullRequest
		if err := json.Unmarshal(raw, &p); err != nil {
			return core.Wrap(core.KindTransient, "github.pulls", err)
		}
		if found == nil && p.Head.Ref == head {
			found = &p
		}
		return nil
	})
	return found, err
}

// EnsurePullRequest implements core.SourceControl. One head has at most one open PR:
// a retry after a lost response finds the PR it created instead of opening another.
func (s *SCM) EnsurePullRequest(ctx context.Context, d core.PullRequestDraft) (core.PullRequest, bool, error) {
	existing, err := s.findOpen(ctx, d.Repo, d.Head)
	if err != nil {
		return core.PullRequest{}, false, err
	}
	if existing != nil {
		pr, err := s.update(ctx, d, *existing)
		return pr, false, err
	}
	var created pullRequest
	body := map[string]any{"title": d.Title, "body": d.Body, "head": d.Head, "base": d.Base, "draft": d.Draft}
	err = s.c.repoCall(ctx, d.Repo.Slug, http.MethodPost, "/repos/"+d.Repo.Slug+"/pulls", body, &created)
	if core.IsKind(err, core.KindInvalid) {
		// A concurrent attempt may have created it between the lookup and this call.
		if again, ferr := s.findOpen(ctx, d.Repo, d.Head); ferr == nil && again != nil {
			pr, uerr := s.update(ctx, d, *again)
			return pr, false, uerr
		}
	}
	if err != nil {
		return core.PullRequest{}, false, err
	}
	return created.canonical(d.Repo.Slug), true, nil
}

func (s *SCM) update(ctx context.Context, d core.PullRequestDraft, p pullRequest) (core.PullRequest, error) {
	var out pullRequest
	path := "/repos/" + d.Repo.Slug + "/pulls/" + strconv.FormatInt(p.Number, 10)
	if err := s.c.repoCall(ctx, d.Repo.Slug, http.MethodPatch, path, map[string]string{"title": d.Title, "body": d.Body}, &out); err != nil {
		return core.PullRequest{}, err
	}
	return out.canonical(d.Repo.Slug), nil
}

// GetPullRequest implements core.SourceControl.
func (s *SCM) GetPullRequest(ctx context.Context, repo core.RepoRef, number string) (core.PullRequest, error) {
	var p pullRequest
	if err := s.c.repoCall(ctx, repo.Slug, http.MethodGet, "/repos/"+repo.Slug+"/pulls/"+number, nil, &p); err != nil {
		return core.PullRequest{}, err
	}
	return p.canonical(repo.Slug), nil
}

// PostPullRequestComment implements core.SourceControl. Conversation comments on pull
// requests are issue comments on GitHub, and share their idempotency.
func (s *SCM) PostPullRequestComment(ctx context.Context, repo core.RepoRef, number string, d core.CommentDraft) error {
	_, err := s.tracker.PostComment(ctx, core.IssueRef{Container: repo.Slug, ExternalID: number}, d)
	return err
}

// PostReview implements core.SourceControl. An approval is published as a comment on
// purpose: Goblins must never be able to satisfy branch protection by itself.
func (s *SCM) PostReview(ctx context.Context, repo core.RepoRef, number string, d core.ReviewDraft) error {
	base := "/repos/" + repo.Slug + "/pulls/" + number + "/reviews"
	body := d.Body
	if d.IdempotencyKey != "" {
		marker := idemMarker(d.IdempotencyKey)
		done := false
		err := s.c.list(ctx, repo.Slug, base+"?per_page=100", func(raw json.RawMessage) error {
			var r struct {
				Body string `json:"body"`
				User user   `json:"user"`
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				return core.Wrap(core.KindTransient, "github.reviews", err)
			}
			done = done || (actorOf(r.User).Type == core.ActorBot && strings.Contains(r.Body, marker))
			return nil
		})
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		body += "\n\n" + marker
	}
	event := "COMMENT"
	if d.Verdict == core.VerdictChangesRequested {
		event = "REQUEST_CHANGES"
	}
	in := map[string]any{"event": event, "body": body}
	if len(d.Comments) > 0 {
		comments := make([]map[string]any, 0, len(d.Comments))
		for _, c := range d.Comments {
			comments = append(comments, map[string]any{"path": c.Path, "line": c.Line, "side": "RIGHT", "body": c.Body})
		}
		in["comments"] = comments
	}
	return s.c.repoCall(ctx, repo.Slug, http.MethodPost, base, in, nil)
}

// ReviewThreads implements core.SourceControl.
func (s *SCM) ReviewThreads(ctx context.Context, repo core.RepoRef, number string) ([]core.ReviewThread, error) {
	out := []core.ReviewThread{}
	var roots []int64
	err := s.c.list(ctx, repo.Slug, "/repos/"+repo.Slug+"/pulls/"+number+"/comments?per_page=100", func(raw json.RawMessage) error {
		var c struct {
			ID        int64  `json:"id"`
			InReplyTo int64  `json:"in_reply_to_id"`
			Path      string `json:"path"`
			Line      int    `json:"line"`
			Body      string `json:"body"`
			User      user   `json:"user"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			return core.Wrap(core.KindTransient, "github.threads", err)
		}
		out = append(out, core.ReviewThread{ID: strconv.FormatInt(c.ID, 10), Path: c.Path, Line: c.Line, Body: c.Body, Author: actorOf(c.User)})
		roots = append(roots, cmp.Or(c.InReplyTo, c.ID))
		return nil
	})
	if err != nil || len(out) == 0 {
		return out, err
	}
	resolved, err := s.resolvedThreads(ctx, repo, number)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Resolved = resolved[strconv.FormatInt(roots[i], 10)]
	}
	return out, nil
}

// threadsQuery asks only for each thread's first comment: replies point to it through
// in_reply_to_id. The comment ID is the BigInt one, as these outgrew a 32-bit integer.
const threadsQuery = `query($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes { isResolved comments(first: 1) { nodes { fullDatabaseId } } }
      }
    }
  }
}`

// resolvedThreads returns the state of each review thread, keyed by its first comment.
// REST lists review comments but has no notion of a thread being resolved.
func (s *SCM) resolvedThreads(ctx context.Context, repo core.RepoRef, number string) (map[string]bool, error) {
	owner, name, ok := strings.Cut(repo.Slug, "/")
	n, err := strconv.Atoi(number)
	if !ok || err != nil {
		return nil, core.Errorf(core.KindInvalid, "github.threads", "bad pull request reference %s#%s", repo.Slug, number)
	}
	out := map[string]bool{}
	vars := map[string]any{"owner": owner, "name": name, "number": n}
	for range maxPages {
		var res struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							IsResolved bool `json:"isResolved"`
							Comments   struct {
								Nodes []struct {
									ID string `json:"fullDatabaseId"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := s.c.graphql(ctx, repo.Slug, threadsQuery, vars, &res); err != nil {
			return nil, err
		}
		threads := res.Repository.PullRequest.ReviewThreads
		for _, t := range threads.Nodes {
			if len(t.Comments.Nodes) > 0 {
				out[t.Comments.Nodes[0].ID] = t.IsResolved
			}
		}
		if !threads.PageInfo.HasNextPage {
			break
		}
		vars["after"] = threads.PageInfo.EndCursor
	}
	return out, nil
}

// StatusChecks implements core.SourceControl.
func (s *SCM) StatusChecks(ctx context.Context, repo core.RepoRef, sha string) ([]core.StatusCheck, error) {
	out := []core.StatusCheck{}
	err := s.c.pages(ctx, repo.Slug, "/repos/"+repo.Slug+"/commits/"+url.PathEscape(sha)+"/check-runs?per_page=100", func(raw json.RawMessage) error {
		var res struct {
			CheckRuns []struct {
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
				HTMLURL    string `json:"html_url"`
			} `json:"check_runs"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return core.Wrap(core.KindTransient, "github.checks", err)
		}
		for _, r := range res.CheckRuns {
			st := core.StatusCheck{Name: r.Name, URL: r.HTMLURL, State: core.CheckPending}
			if r.Status == "completed" {
				st.State = core.CheckFailed
				if r.Conclusion == "success" || r.Conclusion == "neutral" || r.Conclusion == "skipped" {
					st.State = core.CheckPassed
				}
			}
			out = append(out, st)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
