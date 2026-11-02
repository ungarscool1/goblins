package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// review is one submitted review, in a delivery and in a listing.
type review struct {
	ID          int64     `json:"id"`
	State       string    `json:"state"`
	Body        string    `json:"body"`
	User        user      `json:"user"`
	CommitID    string    `json:"commit_id"`
	SubmittedAt time.Time `json:"submitted_at"`
}

func (r review) verdict() core.ReviewVerdict {
	switch strings.ToLower(r.State) {
	case "approved":
		return core.VerdictApprove
	case "changes_requested":
		return core.VerdictChangesRequested
	}
	return core.VerdictComment
}

func reviewEvent(repo string, p pullRequest, action string, r review, actor core.Actor) core.Event {
	c := p.canonical(repo)
	ev := core.Event{Type: core.EventPRReviewed, Subject: prSubject(repo, p.Number), Actor: actor,
		PullRequest: &c, Verdict: r.verdict(), OccurredAt: r.SubmittedAt,
		DedupKey: fmt.Sprintf("github:%s:review:%d", repo, r.ID)}
	if action == "dismissed" {
		ev.Verdict = core.VerdictComment
		ev.DedupKey += ":dismissed"
	}
	return ev
}

func reviewCommentEvent(repo string, p pullRequest, c comment) core.Event {
	cc := c.canonical(core.IssueRef{Container: repo, ExternalID: strconv.FormatInt(p.Number, 10)})
	return core.Event{Type: core.EventReviewComment, Subject: prSubject(repo, p.Number),
		Actor: actorOf(c.User), Comment: &cc, OccurredAt: c.CreatedAt,
		DedupKey: fmt.Sprintf("github:%s:review_comment:%d", repo, c.ID)}
}

// updatedPulls lists the pull requests that changed since a time.
func (s *Source) updatedPulls(ctx context.Context, repo string, since time.Time) ([]pullRequest,
	error) {
	var out []pullRequest
	path := "/repos/" + repo + "/pulls?state=all&sort=updated&direction=desc&per_page=100"
	err := s.c.list(ctx, repo, path, func(raw json.RawMessage) error {
		var p pullRequest
		if err := json.Unmarshal(raw, &p); err != nil {
			return core.Wrap(core.KindTransient, "github.poll", err)
		}
		if p.UpdatedAt.Before(since) {
			return errStopList // newest first, so nothing further can be relevant
		}
		out = append(out, p)
		return nil
	})
	return out, err
}

// pullEvents reports what a poll can observe about one changed pull request, with the same
// identities as webhooks, so that both reporting one fact collapse into one event.
func (s *Source) pullEvents(ctx context.Context, repo string, p pullRequest,
	since time.Time) ([]core.Event, error) {
	author := actorOf(p.User)
	if p.State != "open" {
		return []core.Event{prEvent(repo, core.EventPRClosed, p, author)}, nil
	}
	evs := []core.Event{prEvent(repo, core.EventPROpened, p, author), prEvent(repo,
		core.EventPRSynchronized, p, author)}
	base := "/repos/" + repo + "/pulls/" + strconv.FormatInt(p.Number, 10)
	err := s.c.list(ctx, repo, base+"/reviews?per_page=100", func(raw json.RawMessage) error {
		var r review
		if err := json.Unmarshal(raw, &r); err != nil {
			return core.Wrap(core.KindTransient, "github.poll", err)
		}
		if r.State != "PENDING" {
			evs = append(evs, reviewEvent(repo, p, "submitted", r, actorOf(r.User)))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = s.c.list(ctx, repo, base+"/comments?per_page=100", func(raw json.RawMessage) error {
		var c comment
		if err := json.Unmarshal(raw, &c); err != nil {
			return core.Wrap(core.KindTransient, "github.poll", err)
		}
		if !c.CreatedAt.Before(since) {
			evs = append(evs, reviewCommentEvent(repo, p, c))
		}
		return nil
	})
	return evs, err
}

// ListReviews implements core.SourceControl.
func (s *SCM) ListReviews(ctx context.Context, repo core.RepoRef, number string) ([]core.Review,
	error) {
	own, err := s.c.AppLogin(ctx)
	if err != nil {
		return nil, err
	}
	out := []core.Review{}
	path := "/repos/" + repo.Slug + "/pulls/" + number + "/reviews?per_page=100"
	err = s.c.list(ctx, repo.Slug, path, func(raw json.RawMessage) error {
		var r review
		if err := json.Unmarshal(raw, &r); err != nil {
			return core.Wrap(core.KindTransient, "github.reviews", err)
		}
		if r.State == "PENDING" {
			return nil
		}
		author := actorOf(r.User)
		out = append(out, core.Review{ID: strconv.FormatInt(r.ID, 10), Author: author,
			Verdict: r.verdict(),
			Body:    r.Body, CommitSHA: r.CommitID, Dismissed: r.State == "DISMISSED",
			Own: author.Type == core.ActorBot && author.Login == own})
		return nil
	})
	return out, err
}

// ChangedFiles implements core.SourceControl. A list cut short by the API's limits is an
// error: a gate deciding on part of a change would be worse than no answer.
func (s *SCM) ChangedFiles(ctx context.Context, repo core.RepoRef, number string) ([]string,
	error) {
	out := []string{}
	path := "/repos/" + repo.Slug + "/pulls/" + number + "/files?per_page=100"
	err := s.c.list(ctx, repo.Slug, path, func(raw json.RawMessage) error {
		var f struct {
			Filename string `json:"filename"`
			Previous string `json:"previous_filename"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return core.Wrap(core.KindTransient, "github.files", err)
		}
		out = append(out, f.Filename)
		if f.Previous != "" {
			out = append(out, f.Previous)
		}
		return nil
	})
	return out, err
}

// BranchProtection implements core.SourceControl. An App without administration access gets
// a refusal rather than an answer, which is reported as such: "unknown" is not "unprotected".
func (s *SCM) BranchProtection(ctx context.Context, repo core.RepoRef,
	branch string) (core.BranchProtection, error) {
	var res struct {
		Checks *struct {
			Contexts []string `json:"contexts"`
		} `json:"required_status_checks"`
		Reviews *struct {
			Count int `json:"required_approving_review_count"`
		} `json:"required_pull_request_reviews"`
	}
	path := "/repos/" + repo.Slug + "/branches/" + url.PathEscape(branch) + "/protection"
	err := s.c.repoCall(ctx, repo.Slug, http.MethodGet, path, nil, &res)
	if core.IsKind(err, core.KindNotFound) {
		return core.BranchProtection{}, nil
	}
	if err != nil {
		return core.BranchProtection{}, err
	}
	out := core.BranchProtection{Enabled: true}
	if res.Checks != nil {
		out.RequiredChecks = res.Checks.Contexts
	}
	if res.Reviews != nil {
		out.RequiredApprovals = res.Reviews.Count
	}
	return out, nil
}

// MergePullRequest implements core.SourceControl. Passing the commit makes GitHub refuse if
// the head moved after everything was checked.
func (s *SCM) MergePullRequest(ctx context.Context, repo core.RepoRef, number, sha string) error {
	path := "/repos/" + repo.Slug + "/pulls/" + number + "/merge"
	err := s.c.repoCall(ctx, repo.Slug, http.MethodPut, path, map[string]string{"sha": sha}, nil)
	if core.IsKind(err, core.KindInvalid) {
		return core.Errorf(core.KindConflict, "github.merge", "GitHub refused to merge commit %s",
			sha)
	}
	return err
}

// ReplyToThread implements core.SourceControl. GitHub threads are identified by the comment
// that started them. As with comments, a marker makes a retry after a lost response safe.
func (s *SCM) ReplyToThread(ctx context.Context, repo core.RepoRef, number, threadID string,
	d core.CommentDraft) error {
	own, err := s.c.AppLogin(ctx)
	if err != nil {
		return err
	}
	base := "/repos/" + repo.Slug + "/pulls/" + number + "/comments"
	marker, done := idemMarker(d.IdempotencyKey), false
	err = s.c.list(ctx, repo.Slug, base+"?per_page=100", func(raw json.RawMessage) error {
		var c comment
		if err := json.Unmarshal(raw, &c); err != nil {
			return core.Wrap(core.KindTransient, "github.reply", err)
		}
		a := actorOf(c.User)
		done = done || (a.Type == core.ActorBot && a.Login == own && strings.Contains(c.Body,
			marker))
		return nil
	})
	if err != nil || done {
		return err
	}
	in := map[string]string{"body": d.Body + "\n\n" + marker}
	return s.c.repoCall(ctx, repo.Slug, http.MethodPost,
		base+"/"+url.PathEscape(threadID)+"/replies", in, nil)
}

// PullRequestComments implements core.SourceControl. The general conversation of a pull
// request consists of issue comments on GitHub.
func (s *SCM) PullRequestComments(ctx context.Context, repo core.RepoRef,
	number string) ([]core.Comment, error) {
	return s.tracker.ListComments(ctx, core.IssueRef{Container: repo.Slug, ExternalID: number},
		time.Time{})
}
