package github

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
)

const nativeReviewType = "github-native-review"

// NativeReview asks a reviewer that lives on GitHub, such as a coding agent's GitHub App, for
// a review of one exact commit, and reads what it posted.
//
// Whether a given reviewer responds to a request made by a bot is not something Goblins can
// know, so this does nothing unless an operator has seen it work and said so with `verified`.
// Even then a reviewer that stays silent is simply unavailable: it never holds up the task,
// and the workflow moves on to the next reviewer. Goblins never treats a mention as a promise.
type NativeReview struct {
	name     string
	tracker  *Tracker
	c        *Client
	verified bool
	login    string
	trigger  string
	wait     time.Duration
	every    time.Duration
}

func newNativeReview(name string, c *Client, opts providers.Options) (*NativeReview, error) {
	login, trigger := opts.String("reviewer_login"), opts.String("trigger_comment")
	if login == "" || trigger == "" {
		return nil, core.Errorf(core.KindInvalid, "github.native_review",
			"reviewer_login and trigger_comment are required")
	}
	wait := time.Duration(opts.Int("wait_seconds", 600)) * time.Second
	return &NativeReview{name: name, tracker: NewTracker(name, c), c: c,
		verified: opts.Bool("verified", false), login: login, trigger: trigger, wait: wait,
		every: 30 * time.Second}, nil
}

// Info implements core.Provider.
func (n *NativeReview) Info() core.ProviderInfo {
	return info(core.ProviderReview, n.name)
}

// Review implements core.ReviewProvider.
func (n *NativeReview) Review(ctx context.Context, req core.ReviewRequest) (core.ReviewResult,
	error) {
	if !n.verified {
		return core.ReviewResult{}, core.Errorf(core.KindUnsupported, "github.native_review",
			"native review by %s is not marked as verified on this installation", n.login)
	}
	found, ok, err := n.find(ctx, req)
	if err != nil || ok {
		return found, err
	}
	ref := core.IssueRef{Container: req.Repo.Slug, ExternalID: req.PullRequest.Number}
	draft := core.CommentDraft{IdempotencyKey: "native-review/" + n.login + "/" + req.HeadSHA,
		Body: n.trigger}
	if _, err := n.tracker.PostComment(ctx, ref, draft); err != nil {
		return core.ReviewResult{}, err
	}
	deadline := n.c.now().Add(n.wait)
	for n.c.now().Before(deadline) {
		select {
		case <-ctx.Done():
			return core.ReviewResult{}, core.Wrap(core.KindCancelled, "github.native_review",
				ctx.Err())
		case <-time.After(n.every):
		}
		found, ok, err := n.find(ctx, req)
		if err != nil || ok {
			return found, err
		}
	}
	return core.ReviewResult{}, core.Errorf(core.KindUnsupported, "github.native_review",
		"%s posted no review of %s in time", n.login, req.HeadSHA)
}

// find looks for a review by the configured reviewer on exactly this commit.
func (n *NativeReview) find(ctx context.Context, req core.ReviewRequest) (core.ReviewResult, bool,
	error) {
	base := "/repos/" + req.Repo.Slug + "/pulls/" + req.PullRequest.Number
	var id int64
	var res core.ReviewResult
	err := n.c.list(ctx, req.Repo.Slug, base+"/reviews?per_page=100",
		func(raw json.RawMessage) error {
			var r struct {
				ID     int64  `json:"id"`
				User   user   `json:"user"`
				State  string `json:"state"`
				Body   string `json:"body"`
				Commit string `json:"commit_id"`
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				return core.Wrap(core.KindTransient, "github.native_review", err)
			}
			if r.User.Login == n.login && r.Commit == req.HeadSHA {
				id = r.ID
				res.Summary = r.Body
				// An approval from automation is never an approval to Goblins.
				res.Verdict = core.VerdictComment
				if r.State == "CHANGES_REQUESTED" {
					res.Verdict = core.VerdictChangesRequested
				}
			}
			return nil
		})
	if err != nil || id == 0 {
		return core.ReviewResult{}, false, err
	}
	err = n.c.list(ctx, req.Repo.Slug, base+"/comments?per_page=100",
		func(raw json.RawMessage) error {
			var c struct {
				Review int64  `json:"pull_request_review_id"`
				Path   string `json:"path"`
				Line   int    `json:"line"`
				Body   string `json:"body"`
				User   user   `json:"user"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return core.Wrap(core.KindTransient, "github.native_review", err)
			}
			if c.Review == id {
				res.Findings = append(res.Findings, core.ReviewFinding{Path: c.Path, Line: c.Line,
					Body: c.Body, Severity: badgeSeverity(c.Body), Confidence: core.ConfidenceMedium,
					HeadSHA: req.HeadSHA, Provenance: n.login})
			}
			return nil
		})
	if err != nil {
		return core.ReviewResult{}, false, err
	}
	res.Provider, res.Published = n.login, true
	return res, true, nil
}

var priorityBadge = regexp.MustCompile(`\bP([0-3])\b`)

// badgeSeverity reads a priority tag such as P1 from the start of a comment. Without one, a
// remark counts as medium: worth reading, and not a reason to assume anything is on fire.
func badgeSeverity(body string) core.ReviewSeverity {
	head, _, _ := strings.Cut(body, "\n")
	if m := priorityBadge.FindStringSubmatch(head); m != nil {
		n, _ := strconv.Atoi(m[1])
		return []core.ReviewSeverity{core.ReviewCritical, core.ReviewHigh, core.ReviewMedium,
			core.ReviewLow}[n]
	}
	return core.ReviewMedium
}

var _ core.ReviewProvider = (*NativeReview)(nil)
