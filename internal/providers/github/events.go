package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
)

// MaxWebhookBody bounds what a delivery may occupy in memory.
const MaxWebhookBody = 10 << 20

// Source implements core.EventSource. Webhook support requires a secret, so a
// deployment without inbound connectivity simply does not advertise it.
type Source struct {
	name          string
	c             *Client
	secret        core.Secret
	repositories  []string
	installations []string
}

// SourceOptions configures NewSource.
type SourceOptions struct {
	WebhookSecret core.Secret
	// Repositories and Installations are allow-lists; a delivery outside them is
	// refused. An empty Repositories refuses everything.
	Repositories  []string
	Installations []string
}

// NewSource returns an event source. c may be nil for a webhook-only source.
func NewSource(name string, c *Client, o SourceOptions) *Source {
	return &Source{name: name, c: c, secret: o.WebhookSecret, repositories: o.Repositories, installations: o.Installations}
}

func sourceFromOptions(name string, deps providers.Deps, opts providers.Options) (*Source, error) {
	secret, err := opts.Secret(deps, "webhook_secret")
	if err != nil {
		return nil, err
	}
	var c *Client
	if opts.String("private_key_file") != "" || opts.String("private_key_env") != "" {
		if c, err = clientFromOptions(deps, opts); err != nil {
			return nil, err
		}
	}
	if secret.Empty() && c == nil {
		return nil, core.Errorf(core.KindInvalid, "github.source", "configure webhook_secret, App credentials for polling, or both")
	}
	repos := opts.Strings("repositories")
	if len(repos) == 0 {
		return nil, core.Errorf(core.KindInvalid, "github.source", "repositories allow-list is required")
	}
	return NewSource(name, c, SourceOptions{WebhookSecret: secret, Repositories: repos, Installations: opts.Strings("installation_ids")}), nil
}

// Info implements core.Provider.
func (s *Source) Info() core.ProviderInfo {
	var caps []core.Capability
	if !s.secret.Empty() {
		caps = append(caps, core.CapEventsWebhook)
	}
	if s.c != nil {
		caps = append(caps, core.CapEventsPoll)
	}
	return info(core.ProviderEventSource, s.name, caps...)
}

// HandleWebhook implements core.EventSource.
func (s *Source) HandleWebhook(_ context.Context, req core.WebhookRequest) ([]core.Event, error) {
	if s.secret.Empty() {
		return nil, core.Unsupported(s.Info(), core.CapEventsWebhook)
	}
	if len(req.Body) > MaxWebhookBody {
		return nil, core.Errorf(core.KindInvalid, "github.webhook", "delivery too large")
	}
	if !validSignature(s.secret.Reveal(), req.Body, req.Header("X-Hub-Signature-256")) {
		return nil, core.Errorf(core.KindUnauthorized, "github.webhook", "invalid signature")
	}
	var p payload
	if err := json.Unmarshal(req.Body, &p); err != nil {
		return nil, core.Errorf(core.KindInvalid, "github.webhook", "malformed payload")
	}
	repo := p.Repository.FullName
	if repo == "" {
		return []core.Event{}, nil // e.g. ping or an installation-level event
	}
	if !slices.Contains(s.repositories, repo) {
		return nil, core.Errorf(core.KindForbidden, "github.webhook", "repository is not allowed")
	}
	if len(s.installations) > 0 && !slices.Contains(s.installations, strconv.FormatInt(p.Installation.ID, 10)) {
		return nil, core.Errorf(core.KindForbidden, "github.webhook", "installation is not allowed")
	}
	evs := normalize(req.Header("X-GitHub-Event"), p, repo)
	delivery := req.Header("X-GitHub-Delivery")
	for i := range evs {
		evs[i].Source = s.name
		evs[i].DeliveryID = delivery
	}
	return evs, nil
}

func validSignature(secret string, body []byte, header string) bool {
	hexsig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	want, err := hex.DecodeString(hexsig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}

type pullRequest struct {
	Number  int64  `json:"number"`
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	State   string `json:"state"`
	Draft   bool   `json:"draft"`
	HTMLURL string `json:"html_url"`
	User    user   `json:"user"`
	Head    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (p pullRequest) canonical(repo string) core.PullRequest {
	return core.PullRequest{
		Repo: core.RepoRef{Slug: repo}, Number: strconv.FormatInt(p.Number, 10), URL: p.HTMLURL, Title: p.Title, Head: p.Head.Ref, HeadSHA: p.Head.SHA,
		Base: p.Base.Ref, Draft: p.Draft, Open: p.State == "open", Author: actorOf(p.User),
	}
}

type payload struct {
	Action       string `json:"action"`
	Sender       user   `json:"sender"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Issue       *issue       `json:"issue"`
	Comment     *comment     `json:"comment"`
	PullRequest *pullRequest `json:"pull_request"`
	Label       *label       `json:"label"`
	Review      *struct {
		ID          int64     `json:"id"`
		State       string    `json:"state"`
		SubmittedAt time.Time `json:"submitted_at"`
	} `json:"review"`
	CheckRun *struct {
		ID         int64     `json:"id"`
		Name       string    `json:"name"`
		HeadSHA    string    `json:"head_sha"`
		Conclusion string    `json:"conclusion"`
		HTMLURL    string    `json:"html_url"`
		Completed  time.Time `json:"completed_at"`
		PRs        []struct {
			Number int64 `json:"number"`
		} `json:"pull_requests"`
	} `json:"check_run"`
}

func issueSubject(repo string, n int64) core.Subject {
	return core.Subject{Kind: core.SubjectIssue, Container: repo, ExternalID: strconv.FormatInt(n, 10)}
}

func prSubject(repo string, n int64) core.Subject {
	return core.Subject{Kind: core.SubjectPullRequest, Container: repo, ExternalID: strconv.FormatInt(n, 10)}
}

// The keys below identify facts, so a webhook, its redelivery and a poll that
// observes the same thing collapse into one stored event.

func issueOpenedEvent(repo string, i issue, actor core.Actor) core.Event {
	c := i.canonical(repo)
	return core.Event{Type: core.EventIssueOpened, Subject: issueSubject(repo, i.Number), Actor: actor, Issue: &c, OccurredAt: i.CreatedAt,
		DedupKey: fmt.Sprintf("github:%s:issue:%d:opened", repo, i.Number)}
}

func issueEditedEvent(repo string, i issue, actor core.Actor) core.Event {
	c := i.canonical(repo)
	return core.Event{Type: core.EventIssueEdited, Subject: issueSubject(repo, i.Number), Actor: actor, Issue: &c, OccurredAt: i.UpdatedAt,
		// The time tells apart separate edits that restore earlier text, which must
		// not be dropped as duplicates; the hash, edits within the same second.
		DedupKey: fmt.Sprintf("github:%s:issue:%d:edited:%d:%s", repo, i.Number, i.UpdatedAt.Unix(), core.Digest(c.RequirementsHash())[:16])}
}

func issueClosedEvent(repo string, i issue, actor core.Actor) core.Event {
	c := i.canonical(repo)
	return core.Event{Type: core.EventIssueClosed, Subject: issueSubject(repo, i.Number), Actor: actor, Issue: &c, OccurredAt: i.UpdatedAt,
		DedupKey: fmt.Sprintf("github:%s:issue:%d:closed:%d", repo, i.Number, i.UpdatedAt.Unix())}
}

func issueLabeledEvent(repo string, i issue, name string, actor core.Actor, at time.Time) core.Event {
	c := i.canonical(repo)
	return core.Event{Type: core.EventIssueLabeled, Subject: issueSubject(repo, i.Number), Actor: actor, Issue: &c, Label: name, OccurredAt: at,
		DedupKey: fmt.Sprintf("github:%s:issue:%d:labeled:%s", repo, i.Number, name)}
}

func commentEvent(repo string, n int64, isPR bool, c comment, edited bool) core.Event {
	ref := core.IssueRef{Container: repo, ExternalID: strconv.FormatInt(n, 10)}
	cc := c.canonical(ref)
	subj := issueSubject(repo, n)
	if isPR {
		subj = prSubject(repo, n)
	}
	ev := core.Event{Type: core.EventCommentCreated, Subject: subj, Actor: actorOf(c.User), Comment: &cc, OccurredAt: c.CreatedAt,
		DedupKey: fmt.Sprintf("github:%s:comment:%d:created", repo, c.ID)}
	if edited {
		ev.Type = core.EventCommentEdited
		ev.OccurredAt = c.UpdatedAt
		ev.DedupKey = fmt.Sprintf("github:%s:comment:%d:edited:%d", repo, c.ID, c.UpdatedAt.Unix())
	}
	return ev
}

func prEvent(repo string, typ core.EventType, p pullRequest, actor core.Actor) core.Event {
	c := p.canonical(repo)
	key := "opened"
	switch typ {
	case core.EventPRSynchronized:
		key = "sync:" + p.Head.SHA
	case core.EventPRReopened:
		key = "reopened:" + strconv.FormatInt(p.UpdatedAt.Unix(), 10)
	}
	return core.Event{Type: typ, Subject: prSubject(repo, p.Number), Actor: actor, PullRequest: &c, OccurredAt: p.UpdatedAt,
		DedupKey: fmt.Sprintf("github:%s:pr:%d:%s", repo, p.Number, key)}
}

func normalize(event string, p payload, repo string) []core.Event {
	actor := actorOf(p.Sender)
	switch {
	case event == "issues" && p.Issue != nil && p.Issue.PullRequest == nil:
		switch p.Action {
		case "opened", "reopened":
			return []core.Event{issueOpenedEvent(repo, *p.Issue, actor)}
		case "edited":
			return []core.Event{issueEditedEvent(repo, *p.Issue, actor)}
		case "closed":
			return []core.Event{issueClosedEvent(repo, *p.Issue, actor)}
		case "labeled":
			if p.Label != nil {
				return []core.Event{issueLabeledEvent(repo, *p.Issue, p.Label.Name, actor, p.Issue.UpdatedAt)}
			}
		}
	case event == "issue_comment" && p.Issue != nil && p.Comment != nil:
		isPR := p.Issue.PullRequest != nil
		switch p.Action {
		case "created":
			return []core.Event{commentEvent(repo, p.Issue.Number, isPR, *p.Comment, false)}
		case "edited":
			ev := commentEvent(repo, p.Issue.Number, isPR, *p.Comment, true)
			ev.Actor = actor // the editor, not the author
			return []core.Event{ev}
		}
	case event == "pull_request" && p.PullRequest != nil:
		switch p.Action {
		case "opened":
			return []core.Event{prEvent(repo, core.EventPROpened, *p.PullRequest, actor)}
		case "synchronize":
			return []core.Event{prEvent(repo, core.EventPRSynchronized, *p.PullRequest, actor)}
		case "reopened":
			return []core.Event{prEvent(repo, core.EventPRReopened, *p.PullRequest, actor)}
		}
	case event == "pull_request_review" && p.Action == "submitted" && p.PullRequest != nil && p.Review != nil:
		c := p.PullRequest.canonical(repo)
		verdict := core.VerdictComment
		switch p.Review.State {
		case "approved":
			verdict = core.VerdictApprove
		case "changes_requested":
			verdict = core.VerdictChangesRequested
		}
		return []core.Event{{Type: core.EventPRReviewed, Subject: prSubject(repo, p.PullRequest.Number), Actor: actor, PullRequest: &c, Verdict: verdict,
			OccurredAt: p.Review.SubmittedAt, DedupKey: fmt.Sprintf("github:%s:review:%d", repo, p.Review.ID)}}
	case event == "check_run" && p.Action == "completed" && p.CheckRun != nil:
		state := core.CheckFailed
		if p.CheckRun.Conclusion == "success" || p.CheckRun.Conclusion == "neutral" || p.CheckRun.Conclusion == "skipped" {
			state = core.CheckPassed
		}
		var out []core.Event
		for _, pr := range p.CheckRun.PRs {
			out = append(out, core.Event{Type: core.EventCheckCompleted, Subject: prSubject(repo, pr.Number), Actor: actor, OccurredAt: p.CheckRun.Completed,
				Check:    &core.StatusCheck{Name: p.CheckRun.Name, State: state, URL: p.CheckRun.HTMLURL},
				DedupKey: fmt.Sprintf("github:%s:check:%d:%d", repo, p.CheckRun.ID, pr.Number)})
		}
		return out
	}
	return []core.Event{}
}

// Poll implements core.EventSource as read-only reconciliation. The cursor is a
// timestamp; `since` is inclusive, and dedup makes the overlap harmless.
func (s *Source) Poll(ctx context.Context, req core.PollRequest) (core.PollResult, error) {
	if s.c == nil {
		return core.PollResult{}, core.Unsupported(s.Info(), core.CapEventsPoll)
	}
	if !slices.Contains(s.repositories, req.Container) {
		return core.PollResult{}, core.Errorf(core.KindForbidden, "github.poll", "repository is not allowed")
	}
	repo := req.Container
	since := req.Since
	if req.Cursor != "" {
		t, err := time.Parse(time.RFC3339, req.Cursor)
		if err != nil {
			return core.PollResult{}, core.Errorf(core.KindInvalid, "github.poll", "invalid cursor")
		}
		since = t
	}
	latest := since
	seen := func(t time.Time) {
		if t.After(latest) {
			latest = t
		}
	}
	// A listing sorted oldest first that is cut short can continue from where it stopped,
	// so long as the cursor does not pass that point.
	var hold time.Time
	resumeFrom := func(err error, last time.Time) error {
		if !errors.Is(err, errListTruncated) || !last.After(since) {
			return err
		}
		if hold.IsZero() || last.Before(hold) {
			hold = last
		}
		return nil
	}
	isPR := map[int64]bool{}
	evs := []core.Event{}
	q := url.Values{"per_page": {"100"}, "sort": {"updated"}, "direction": {"asc"}, "since": {since.UTC().Format(time.RFC3339)}}

	stateAll := url.Values{"state": {"all"}}
	for k, v := range q {
		stateAll[k] = v
	}
	var lastIssue time.Time
	err := s.c.list(ctx, repo, "/repos/"+repo+"/issues?"+stateAll.Encode(), func(raw json.RawMessage) error {
		var i issue
		if err := json.Unmarshal(raw, &i); err != nil {
			return core.Wrap(core.KindTransient, "github.poll", err)
		}
		seen(i.UpdatedAt)
		lastIssue = i.UpdatedAt
		isPR[i.Number] = i.PullRequest != nil
		if i.PullRequest != nil {
			return nil
		}
		actor := actorOf(i.User)
		if !i.CreatedAt.Before(since) {
			evs = append(evs, issueOpenedEvent(repo, i, actor))
		}
		if i.UpdatedAt.After(i.CreatedAt) {
			// Polling cannot tell who edited, so this is attributed to the author, whose
			// permission gates it. The workflow ignores it if the text did not change.
			evs = append(evs, issueEditedEvent(repo, i, actor))
		}
		if i.State == "closed" {
			evs = append(evs, issueClosedEvent(repo, i, actor))
		}
		return nil
	})
	if err := resumeFrom(err, lastIssue); err != nil {
		return core.PollResult{}, err
	}

	var lastComment time.Time
	err = s.c.list(ctx, repo, "/repos/"+repo+"/issues/comments?"+q.Encode(), func(raw json.RawMessage) error {
		var c comment
		if err := json.Unmarshal(raw, &c); err != nil {
			return core.Wrap(core.KindTransient, "github.poll", err)
		}
		seen(c.UpdatedAt)
		lastComment = c.UpdatedAt
		n, err := strconv.ParseInt(c.IssueURL[strings.LastIndex(c.IssueURL, "/")+1:], 10, 64)
		if err != nil {
			return nil //nolint:nilerr // a comment we cannot attribute is not actionable
		}
		pr, known := isPR[n]
		if !known {
			if pr, err = s.parentIsPR(ctx, repo, n); core.IsKind(err, core.KindNotFound) {
				return nil // the parent is gone, so there is nothing to act on
			} else if err != nil {
				return err
			}
			isPR[n] = pr
		}
		// The text at creation cannot be recovered once edited. Reporting today's text as
		// a new comment would let an edit pass as an approval or a command.
		if c.UpdatedAt.After(c.CreatedAt) {
			evs = append(evs, commentEvent(repo, n, pr, c, true))
		} else {
			evs = append(evs, commentEvent(repo, n, pr, c, false))
		}
		return nil
	})
	if err := resumeFrom(err, lastComment); err != nil {
		return core.PollResult{}, err
	}

	err = s.c.list(ctx, repo, "/repos/"+repo+"/issues/events?per_page=100", func(raw json.RawMessage) error {
		var e struct {
			Event     string    `json:"event"`
			Actor     user      `json:"actor"`
			Label     label     `json:"label"`
			Issue     *issue    `json:"issue"`
			CreatedAt time.Time `json:"created_at"`
		}
		if err := json.Unmarshal(raw, &e); err != nil {
			return core.Wrap(core.KindTransient, "github.poll", err)
		}
		if e.CreatedAt.Before(since) {
			return errStopList // newest first, so nothing further can be relevant
		}
		if e.Event == "labeled" && e.Issue != nil && e.Issue.PullRequest == nil {
			seen(e.CreatedAt)
			evs = append(evs, issueLabeledEvent(repo, *e.Issue, e.Label.Name, actorOf(e.Actor), e.CreatedAt))
		}
		return nil
	})
	if err != nil {
		return core.PollResult{}, err
	}

	err = s.c.list(ctx, repo, "/repos/"+repo+"/pulls?state=open&sort=updated&direction=desc&per_page=100", func(raw json.RawMessage) error {
		var p pullRequest
		if err := json.Unmarshal(raw, &p); err != nil {
			return core.Wrap(core.KindTransient, "github.poll", err)
		}
		if p.UpdatedAt.Before(since) {
			return errStopList // newest first, so nothing further can be relevant
		}
		seen(p.UpdatedAt)
		evs = append(evs, prEvent(repo, core.EventPROpened, p, actorOf(p.User)))
		return nil
	})
	if err != nil {
		return core.PollResult{}, err
	}
	for i := range evs {
		evs[i].Source = s.name
	}
	if !hold.IsZero() && hold.Before(latest) {
		latest = hold
	}
	return core.PollResult{Events: evs, NextCursor: latest.UTC().Format(time.RFC3339)}, nil
}

// parentIsPR looks up what a comment belongs to, as the comment itself does not say.
func (s *Source) parentIsPR(ctx context.Context, repo string, n int64) (bool, error) {
	var i issue
	if err := s.c.repoCall(ctx, repo, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d", repo, n), nil, &i); err != nil {
		return false, err
	}
	return i.PullRequest != nil, nil
}
