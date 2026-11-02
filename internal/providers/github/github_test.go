package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
	"github.com/ungarscool1/goblins/internal/providers/contract"
	gh "github.com/ungarscool1/goblins/internal/providers/github"
	"github.com/ungarscool1/goblins/internal/providers/github/fakegithub"
	"github.com/ungarscool1/goblins/internal/workflow"
)

const (
	repo   = "acme/app"
	secret = "whsec-test"
)

var (
	alice = fakegithub.Human(1, "alice")
	ctx   = context.Background()
)

func setup(t *testing.T) (*fakegithub.Server, *gh.Client) {
	t.Helper()
	srv := fakegithub.New(t)
	srv.AddRepo(repo)
	c, err := gh.NewClient(gh.ClientOptions{API: srv.URL, AppID: "12345", PrivateKey: srv.KeyPEM(), Now: func() time.Time { return srv.Now() }})
	if err != nil {
		t.Fatal(err)
	}
	return srv, c
}

func TestTrackerContract(t *testing.T) {
	contract.IssueTracker(t, func(t *testing.T) contract.TrackerHarness {
		srv, c := setup(t)
		return contract.TrackerHarness{
			Tracker: gh.NewTracker("gh", c), Container: repo,
			Seed: func(_ *testing.T, title, body string) core.IssueRef {
				i := srv.AddIssue(repo, title, body, alice)
				return core.IssueRef{Container: repo, ExternalID: fmt.Sprint(i.Number)}
			},
			SeedComment: func(t *testing.T, ref core.IssueRef, login, body string) core.Comment {
				var n int64
				_, _ = fmt.Sscan(ref.ExternalID, &n)
				c := srv.AddComment(repo, n, fakegithub.Human(2, login), body)
				return core.Comment{ID: fmt.Sprint(c.ID)}
			},
			Grant: func(_ *testing.T, login string, p core.Permission) { srv.Grant(repo, login, p.String()) },
		}
	})
}

func TestTokensAreScopedAndCached(t *testing.T) {
	srv, c := setup(t)
	if _, _, err := c.Token(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Token(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if len(srv.TokenRepos) != 1 || len(srv.TokenRepos[0]) != 1 || srv.TokenRepos[0][0] != "app" {
		t.Fatalf("token requests = %v", srv.TokenRepos)
	}
}

func TestTokenRotatesBeforeExpiry(t *testing.T) {
	srv, c := setup(t)
	now := time.Now()
	srv.Now = func() time.Time { return now }
	first, _, err := c.Token(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(59 * time.Minute)
	second, _, err := c.Token(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if first.Reveal() == second.Reveal() {
		t.Fatal("expiring token was reused")
	}
}

func TestErrorClassification(t *testing.T) {
	srv, c := setup(t)
	tr := gh.NewTracker("gh", c)
	ref := core.IssueRef{Container: repo, ExternalID: "1"}
	srv.AddIssue(repo, "t", "b", alice)
	for status, kind := range map[int]core.ErrorKind{
		500: core.KindTransient, 502: core.KindTransient, 429: core.KindTransient,
		403: core.KindForbidden, 404: core.KindNotFound, 422: core.KindInvalid, 409: core.KindConflict,
	} {
		if _, _, err := c.Token(ctx, repo); err != nil {
			t.Fatal(err)
		}
		srv.FailOnce("GET /repos/acme/app/issues/1", status)
		if _, err := tr.GetIssue(ctx, ref); !core.IsKind(err, kind) {
			t.Errorf("%d: %v, want %s", status, err, kind)
		}
	}
	if _, err := tr.GetIssue(ctx, core.IssueRef{Container: "other/repo", ExternalID: "1"}); !core.IsKind(err, core.KindNotFound) {
		t.Errorf("not installed: %v", err)
	}
}

func TestPermissionRolesAndFailClosed(t *testing.T) {
	srv, c := setup(t)
	tr := gh.NewTracker("gh", c)
	ref := core.IssueRef{Container: repo, ExternalID: "1"}
	for role, want := range map[string]core.Permission{"admin": core.PermissionAdmin, "maintain": core.PermissionWrite, "write": core.PermissionWrite, "triage": core.PermissionTriage, "read": core.PermissionRead} {
		srv.Grant(repo, role+"-user", role)
		got, err := tr.ActorPermission(ctx, ref, core.Actor{Login: role + "-user"})
		if err != nil || got != want {
			t.Errorf("%s = %s, %v", role, got, err)
		}
	}
	if got, err := tr.ActorPermission(ctx, ref, core.Actor{Login: "../../evil"}); err != nil || got != core.PermissionNone {
		t.Errorf("outsider = %s, %v", got, err)
	}
}

func TestIdempotencyMarkerCannotBeForgedByAPerson(t *testing.T) {
	srv, c := setup(t)
	tr := gh.NewTracker("gh", c)
	i := srv.AddIssue(repo, "t", "b", alice)
	ref := core.IssueRef{Container: repo, ExternalID: fmt.Sprint(i.Number)}
	posted, err := tr.PostComment(ctx, ref, core.CommentDraft{IdempotencyKey: "k", Body: "first"})
	if err != nil {
		t.Fatal(err)
	}
	marker := posted.Body[strings.Index(posted.Body, "<!--"):]
	if !strings.HasPrefix(marker, "<!-- goblins:idem:") {
		t.Fatalf("marker = %q", marker)
	}
	// A person pastes the marker of a future comment to suppress it. The marker
	// is learned from another issue, as anyone could by reading a public one.
	other := srv.AddIssue(repo, "other", "b", alice)
	learned, err := tr.PostComment(ctx, core.IssueRef{Container: repo, ExternalID: fmt.Sprint(other.Number)}, core.CommentDraft{IdempotencyKey: "k2", Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	srv.AddComment(repo, i.Number, alice, learned.Body[strings.Index(learned.Body, "<!--"):])
	if _, err := tr.PostComment(ctx, ref, core.CommentDraft{IdempotencyKey: "k2", Body: "must appear"}); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range srv.Comments(repo, i.Number) {
		found = found || (strings.Contains(c.Body, "must appear") && c.User.Login == fakegithub.BotUser.Login)
	}
	if !found {
		t.Fatal("forged marker suppressed a comment")
	}
}

func delivery(t *testing.T, event string, payload any, secretOverride ...string) core.WebhookRequest {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	s := secret
	if len(secretOverride) > 0 {
		s = secretOverride[0]
	}
	return core.WebhookRequest{Body: body, Headers: map[string][]string{
		"X-GitHub-Event": {event}, "X-GitHub-Delivery": {"d-1"}, "X-Hub-Signature-256": {fakegithub.Sign(s, body)},
	}}
}

func repoField(name string) map[string]any { return map[string]any{"full_name": name} }

func newSource(c *gh.Client) *gh.Source {
	return gh.NewSource("gh", c, gh.SourceOptions{WebhookSecret: core.NewSecret(secret), Repositories: []string{repo}, Installations: []string{"77"}})
}

func issuePayload(action string, extra map[string]any) map[string]any {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := map[string]any{
		"action": action, "repository": repoField(repo), "installation": map[string]any{"id": 77},
		"sender": map[string]any{"id": 1, "login": "alice", "type": "User"},
		"issue":  map[string]any{"number": 5, "title": "T", "body": "B", "state": "open", "user": map[string]any{"id": 1, "login": "alice", "type": "User"}, "created_at": now, "updated_at": now},
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func TestEventSourceContract(t *testing.T) {
	contract.EventSource(t, func(t *testing.T) contract.EventSourceHarness {
		srv, c := setup(t)
		return contract.EventSourceHarness{
			Source: newSource(c), Container: repo,
			Valid:  delivery(t, "issues", issuePayload("opened", nil)),
			Forged: delivery(t, "issues", issuePayload("opened", nil), "wrong-secret"),
			Emit:   func(*testing.T) { srv.AddIssue(repo, "polled", "b", alice) },
		}
	})
}

func TestWebhookSecurity(t *testing.T) {
	src := newSource(nil)
	good := delivery(t, "issues", issuePayload("opened", nil))

	tampered := good
	tampered.Body = []byte(strings.Replace(string(good.Body), `"title":"T"`, `"title":"evil"`, 1))
	if _, err := src.HandleWebhook(ctx, tampered); !core.IsKind(err, core.KindUnauthorized) {
		t.Fatalf("tampered: %v", err)
	}
	for _, sig := range []string{"", "sha1=abc", "sha256=zz", "sha256=00"} {
		bad := core.WebhookRequest{Body: good.Body, Headers: map[string][]string{"X-Hub-Signature-256": {sig}, "X-GitHub-Event": {"issues"}}}
		if _, err := src.HandleWebhook(ctx, bad); !core.IsKind(err, core.KindUnauthorized) {
			t.Fatalf("signature %q: %v", sig, err)
		}
	}
	other := issuePayload("opened", map[string]any{"repository": repoField("evil/repo")})
	if _, err := src.HandleWebhook(ctx, delivery(t, "issues", other)); !core.IsKind(err, core.KindForbidden) {
		t.Fatalf("unauthorized repository: %v", err)
	}
	otherInst := issuePayload("opened", map[string]any{"installation": map[string]any{"id": 1}})
	if _, err := src.HandleWebhook(ctx, delivery(t, "issues", otherInst)); !core.IsKind(err, core.KindForbidden) {
		t.Fatalf("unauthorized installation: %v", err)
	}
	big := delivery(t, "issues", issuePayload("opened", nil))
	big.Body = make([]byte, gh.MaxWebhookBody+1)
	if _, err := src.HandleWebhook(ctx, big); !core.IsKind(err, core.KindInvalid) {
		t.Fatalf("oversized: %v", err)
	}
	notJSON := []byte("not json")
	bad := core.WebhookRequest{Body: notJSON, Headers: map[string][]string{"X-Hub-Signature-256": {fakegithub.Sign(secret, notJSON)}}}
	if _, err := src.HandleWebhook(ctx, bad); !core.IsKind(err, core.KindInvalid) {
		t.Fatalf("malformed: %v", err)
	}
	noSecret := gh.NewSource("gh", nil, gh.SourceOptions{Repositories: []string{repo}})
	if _, err := noSecret.HandleWebhook(ctx, good); !core.IsKind(err, core.KindUnsupported) {
		t.Fatalf("no secret: %v", err)
	}
	if noSecret.Info().Has(core.CapEventsWebhook) {
		t.Fatal("advertises webhooks without a secret")
	}
}

func TestWebhookNormalization(t *testing.T) {
	src := newSource(nil)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pr := map[string]any{"number": 9, "title": "P", "state": "open", "draft": true, "user": map[string]any{"login": "alice", "type": "User"},
		"head": map[string]any{"ref": "feat", "sha": "abc"}, "base": map[string]any{"ref": "main"}, "updated_at": now}
	cases := []struct {
		name    string
		event   string
		payload map[string]any
		want    core.EventType
		kind    core.SubjectKind
	}{
		{"opened", "issues", issuePayload("opened", nil), core.EventIssueOpened, core.SubjectIssue},
		{"edited", "issues", issuePayload("edited", nil), core.EventIssueEdited, core.SubjectIssue},
		{"closed", "issues", issuePayload("closed", nil), core.EventIssueClosed, core.SubjectIssue},
		{"labeled", "issues", issuePayload("labeled", map[string]any{"label": map[string]any{"name": "goblins"}}), core.EventIssueLabeled, core.SubjectIssue},
		{"comment", "issue_comment", issuePayload("created", map[string]any{"comment": map[string]any{"id": 3, "body": "hi", "user": map[string]any{"login": "alice", "type": "User"}, "created_at": now, "updated_at": now}}), core.EventCommentCreated, core.SubjectIssue},
		{"comment edit", "issue_comment", issuePayload("edited", map[string]any{"comment": map[string]any{"id": 3, "body": "hi", "user": map[string]any{"login": "alice", "type": "User"}, "created_at": now, "updated_at": now}}), core.EventCommentEdited, core.SubjectIssue},
		{"pr opened", "pull_request", map[string]any{"action": "opened", "repository": repoField(repo), "installation": map[string]any{"id": 77}, "sender": map[string]any{"login": "alice", "type": "User"}, "pull_request": pr}, core.EventPROpened, core.SubjectPullRequest},
		{"pr sync", "pull_request", map[string]any{"action": "synchronize", "repository": repoField(repo), "installation": map[string]any{"id": 77}, "sender": map[string]any{"login": "alice", "type": "User"}, "pull_request": pr}, core.EventPRSynchronized, core.SubjectPullRequest},
		{"pr review", "pull_request_review", map[string]any{"action": "submitted", "repository": repoField(repo), "installation": map[string]any{"id": 77}, "pull_request": pr, "review": map[string]any{"id": 8, "state": "changes_requested", "submitted_at": now}}, core.EventPRReviewed, core.SubjectPullRequest},
		{"check", "check_run", map[string]any{"action": "completed", "repository": repoField(repo), "installation": map[string]any{"id": 77}, "check_run": map[string]any{"id": 7, "name": "ci", "conclusion": "failure", "pull_requests": []map[string]any{{"number": 9}}}}, core.EventCheckCompleted, core.SubjectPullRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evs, err := src.HandleWebhook(ctx, delivery(t, tc.event, tc.payload))
			if err != nil || len(evs) != 1 {
				t.Fatalf("events = %+v, %v", evs, err)
			}
			if evs[0].Type != tc.want || evs[0].Subject.Kind != tc.kind || evs[0].DedupKey == "" || evs[0].DeliveryID != "d-1" {
				t.Fatalf("event = %+v", evs[0])
			}
		})
	}
	// Irrelevant but valid deliveries produce nothing.
	for _, tc := range []struct {
		event   string
		payload map[string]any
	}{
		{"ping", map[string]any{"zen": "x"}},
		{"star", issuePayload("created", nil)},
		{"issues", issuePayload("pinned", nil)},
	} {
		evs, err := src.HandleWebhook(ctx, delivery(t, tc.event, tc.payload))
		if err != nil || len(evs) != 0 {
			t.Fatalf("%s: %+v, %v", tc.event, evs, err)
		}
	}
	// Our own comment comes through marked as automation, which stops any loop.
	bot := issuePayload("created", map[string]any{
		"sender":  map[string]any{"login": "goblins-app[bot]", "type": "Bot"},
		"comment": map[string]any{"id": 4, "body": "hello", "user": map[string]any{"login": "goblins-app[bot]", "type": "Bot"}, "created_at": now, "updated_at": now},
	})
	evs, err := src.HandleWebhook(ctx, delivery(t, "issue_comment", bot))
	if err != nil || len(evs) != 1 || evs[0].Comment.Author.Type != core.ActorBot {
		t.Fatalf("bot comment = %+v, %v", evs, err)
	}
}

func TestPollMatchesWebhookIdentity(t *testing.T) {
	srv, c := setup(t)
	src := newSource(c)
	start := time.Now().Add(-time.Minute)
	i := srv.AddIssue(repo, "T", "B", alice)
	cm := srv.AddComment(repo, i.Number, alice, "hello")
	srv.AddLabel(repo, i.Number, "goblins", fakegithub.Human(3, "maintainer"))

	res, err := src.Poll(ctx, core.PollRequest{Container: repo, Since: start})
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]core.Event{}
	for _, e := range res.Events {
		keys[e.DedupKey] = e
	}
	opened := fmt.Sprintf("github:%s:issue:%d:opened", repo, i.Number)
	commented := fmt.Sprintf("github:%s:comment:%d:created", repo, cm.ID)
	labeled := fmt.Sprintf("github:%s:issue:%d:labeled:goblins:%d", repo, i.Number, srv.GetIssue(repo, i.Number).UpdatedAt.Unix())
	for _, k := range []string{opened, commented, labeled} {
		if _, ok := keys[k]; !ok {
			t.Errorf("missing %s in %v", k, res.Events)
		}
	}
	if keys[labeled].Actor.Login != "maintainer" {
		t.Errorf("labeler lost: %+v", keys[labeled].Actor)
	}

	// The webhook for the same issue has the same identity, so one is dropped.
	pl := issuePayload("opened", map[string]any{"issue": map[string]any{"number": i.Number, "title": "T", "body": "B", "state": "open", "user": alice, "created_at": i.CreatedAt, "updated_at": i.UpdatedAt}})
	evs, err := src.HandleWebhook(ctx, delivery(t, "issues", pl))
	if err != nil || evs[0].DedupKey != opened {
		t.Fatalf("webhook key = %+v, %v", evs, err)
	}

	if _, err := time.Parse(time.RFC3339, res.NextCursor); err != nil {
		t.Fatalf("cursor = %q", res.NextCursor)
	}
	if _, err := src.Poll(ctx, core.PollRequest{Container: repo, Cursor: "garbage"}); !core.IsKind(err, core.KindInvalid) {
		t.Fatalf("bad cursor: %v", err)
	}
	if _, err := src.Poll(ctx, core.PollRequest{Container: "evil/repo"}); !core.IsKind(err, core.KindForbidden) {
		t.Fatalf("unlisted repo: %v", err)
	}
	if _, err := newSource(nil).Poll(ctx, core.PollRequest{Container: repo}); !core.IsKind(err, core.KindUnsupported) {
		t.Fatalf("no client: %v", err)
	}
}

func pollKeys(t *testing.T, src *gh.Source, since time.Time) map[string]core.Event {
	t.Helper()
	res, err := src.Poll(ctx, core.PollRequest{Container: repo, Since: since})
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]core.Event{}
	for _, e := range res.Events {
		keys[e.DedupKey] = e
	}
	return keys
}

// advancingClock gives each read of the time its own second, as if work took a while.
func advancingClock(srv *fakegithub.Server) {
	var mu sync.Mutex
	at := time.Now().Add(-time.Hour)
	srv.Now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		at = at.Add(time.Second)
		return at
	}
}

// pollInto feeds what a poll finds through the workflow, as the service would.
func pollInto(t *testing.T, src *gh.Source, task *core.Task, since time.Time, p workflow.Policy) *core.Task {
	t.Helper()
	res, err := src.Poll(ctx, core.PollRequest{Container: repo, Since: since})
	if err != nil {
		t.Fatal(err)
	}
	facts := workflow.Facts{ActorPermission: core.PermissionWrite, Now: time.Now()}
	for _, e := range res.Events {
		if d := workflow.Decide(task, e, facts, p); d.Task != nil {
			task = d.Task
		}
	}
	return task
}

func TestPollingAloneStartsWorkFromAnIntakeLabel(t *testing.T) {
	p := workflow.DefaultPolicy()
	p.IntakeLabel = "goblins"
	maintainer := fakegithub.Human(3, "maintainer")

	t.Run("applied when the issue was created", func(t *testing.T) {
		srv, c := setup(t)
		src := newSource(c)
		start := time.Now().Add(-time.Minute)
		i := srv.AddIssue(repo, "T", "B", alice)
		srv.AddLabel(repo, i.Number, "goblins", maintainer)
		if task := pollInto(t, src, nil, start, p); task == nil || task.State != core.StateInvestigating {
			t.Fatalf("task = %+v", task)
		}
	})
	t.Run("applied later", func(t *testing.T) {
		srv, c := setup(t)
		src := newSource(c)
		start := time.Now().Add(-time.Minute)
		i := srv.AddIssue(repo, "T", "B", alice)
		if task := pollInto(t, src, nil, start, p); task != nil {
			t.Fatalf("started without the label: %+v", task)
		}
		srv.AddLabel(repo, i.Number, "goblins", maintainer)
		if task := pollInto(t, src, nil, start, p); task == nil || task.State != core.StateInvestigating {
			t.Fatalf("task = %+v", task)
		}
	})
}

func TestApplyingALabelAgainIsANewEvent(t *testing.T) {
	srv, c := setup(t)
	advancingClock(srv)
	src := newSource(c)
	start := srv.Now()
	i := srv.AddIssue(repo, "T", "B", alice)
	srv.AddLabel(repo, i.Number, "goblins", alice)
	srv.AddLabel(repo, i.Number, "goblins", fakegithub.Human(3, "maintainer"))
	var labeled []core.Event
	for _, e := range pollKeys(t, src, start) {
		if e.Type == core.EventIssueLabeled {
			labeled = append(labeled, e)
		}
	}
	if len(labeled) != 2 {
		t.Fatalf("labeled events = %+v", labeled)
	}
	// The same fact seen again still has one identity.
	again := pollKeys(t, src, start)
	for _, e := range labeled {
		if _, ok := again[e.DedupKey]; !ok {
			t.Fatalf("key changed between polls: %s", e.DedupKey)
		}
	}
}

func TestEditRestoringEarlierTextIsANewEvent(t *testing.T) {
	srv, c := setup(t)
	advancingClock(srv)
	src := newSource(c)
	start := srv.Now()
	i := srv.AddIssue(repo, "T", "A", alice)
	first := srv.EditIssue(repo, i.Number, "A again")
	key := func() string {
		for k, e := range pollKeys(t, src, start) {
			if e.Type == core.EventIssueEdited {
				return k
			}
		}
		t.Fatal("no edit event")
		return ""
	}
	seen := key()
	srv.EditIssue(repo, i.Number, "B")
	srv.EditIssue(repo, i.Number, first.Body)
	if again := key(); again == seen {
		t.Fatalf("the edit back to earlier text was deduplicated: %s", again)
	}
}

func TestPollNeverReportsAnEditedCommentAsNew(t *testing.T) {
	srv, c := setup(t)
	advancingClock(srv)
	src := newSource(c)
	start := srv.Now()
	i := srv.AddIssue(repo, "T", "B", alice)
	cm := srv.AddComment(repo, i.Number, alice, "just a remark")
	srv.EditComment(repo, i.Number, cm.ID, "/approve-design 1")
	var found bool
	for _, e := range pollKeys(t, src, start) {
		if e.Type == core.EventCommentCreated {
			t.Fatalf("edited text reported as a new comment: %+v", e.Comment)
		}
		found = found || e.Type == core.EventCommentEdited
	}
	if !found {
		t.Fatal("the edit was lost")
	}
}

func TestPollClassifiesPullRequestComments(t *testing.T) {
	srv, c := setup(t)
	src := newSource(c)
	start := time.Now().Add(-time.Minute)
	p := srv.AddPullRequest(repo, "PR", alice)
	cm := srv.AddComment(repo, p.Number, alice, "/goblins cancel")
	key := fmt.Sprintf("github:%s:comment:%d:created", repo, cm.ID)

	e, ok := pollKeys(t, src, start)[key]
	if !ok || e.Subject.Kind != core.SubjectPullRequest {
		t.Fatalf("event = %+v, found %v", e, ok)
	}
	// Also when the parent falls outside the issue listing, which is filtered by its time.
	srv.SetPullRequestUpdated(repo, p.Number, time.Now().Add(-time.Hour))
	e, ok = pollKeys(t, src, start)[key]
	if !ok || e.Subject.Kind != core.SubjectPullRequest {
		t.Fatalf("event after lookup = %+v, found %v", e, ok)
	}
}

func TestPollResumesAfterATruncatedListing(t *testing.T) {
	srv, c := setup(t)
	advancingClock(srv)
	src := newSource(c)
	start := srv.Now()
	i := srv.AddIssue(repo, "T", "B", alice)
	var last fakegithub.Comment
	for range 2100 {
		last = srv.AddComment(repo, i.Number, alice, "x")
	}
	res, err := src.Poll(ctx, core.PollRequest{Container: repo, Since: start})
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := time.Parse(time.RFC3339, res.NextCursor)
	if err != nil || !cursor.Before(last.UpdatedAt) {
		t.Fatalf("cursor %s passed comments it never read, up to %s (%v)", res.NextCursor, last.UpdatedAt, err)
	}
	// The next poll picks up from there and reaches the end.
	res, err = src.Poll(ctx, core.PollRequest{Container: repo, Cursor: res.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("github:%s:comment:%d:created", repo, last.ID)
	if !slices.ContainsFunc(res.Events, func(e core.Event) bool { return e.DedupKey == want }) {
		t.Fatalf("last comment not reached; %d events", len(res.Events))
	}
}

func TestTruncatedCommentListingIsAnError(t *testing.T) {
	srv, c := setup(t)
	i := srv.AddIssue(repo, "T", "B", alice)
	for range 2100 {
		srv.AddComment(repo, i.Number, alice, "x")
	}
	tr := gh.NewTracker("t", c)
	ref := core.IssueRef{Container: repo, ExternalID: strconv.FormatInt(i.Number, 10)}
	if _, err := tr.ListComments(ctx, ref, time.Time{}); err == nil {
		t.Fatal("an incomplete listing was reported as complete")
	}
}

func TestRegistryFactories(t *testing.T) {
	srv, _ := setup(t)
	r := providers.NewRegistry()
	gh.Register(r)
	deps := providers.Deps{Getenv: func(k string) (string, bool) {
		switch k {
		case "KEY":
			return string(srv.KeyPEM()), true
		case "HOOK":
			return secret, true
		}
		return "", false
	}}
	opts := providers.Options{"app_id": 12345, "private_key_env": "KEY", "api_url": srv.URL}
	if _, err := providers.As[core.IssueTracker](r, providers.KindTracker, "github", "t", deps, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := providers.As[core.IssueTracker](r, providers.KindTracker, "github", "t", deps, providers.Options{"app_id": 1}); !core.IsKind(err, core.KindInvalid) {
		t.Fatalf("no key: %v", err)
	}
	src, err := providers.As[core.EventSource](r, providers.KindEventSource, "github", "e", deps, providers.Options{"webhook_secret_env": "HOOK", "repositories": []any{repo}})
	if err != nil || !src.Info().Has(core.CapEventsWebhook) || src.Info().Has(core.CapEventsPoll) {
		t.Fatalf("webhook-only source: %+v, %v", src, err)
	}
	if _, err := providers.As[core.EventSource](r, providers.KindEventSource, "github", "e", deps, providers.Options{"webhook_secret_env": "HOOK"}); !core.IsKind(err, core.KindInvalid) {
		t.Fatalf("no allow-list: %v", err)
	}
}

func TestPullRequestWebhooksNormalize(t *testing.T) {
	srv, _ := setup(t)
	bob := fakegithub.Human(7, "bob")
	pr := srv.AddPullRequest(repo, "t", bob)
	source := gh.NewSource("s", nil, gh.SourceOptions{WebhookSecret: core.NewSecret("x"), Repositories: []string{repo}})
	deliver := func(event string, body map[string]any) []core.Event {
		t.Helper()
		body["repository"] = map[string]any{"full_name": repo}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		evs, err := source.HandleWebhook(ctx, core.WebhookRequest{Body: raw, Headers: map[string][]string{
			"X-Hub-Signature-256": {fakegithub.Sign("x", raw)}, "X-GitHub-Event": {event}}})
		if err != nil || len(evs) != 1 {
			t.Fatalf("%s: %+v, %v", event, evs, err)
		}
		return evs
	}

	review := map[string]any{"id": 9, "state": "changes_requested", "commit_id": "abc", "user": bob}
	ev := deliver("pull_request_review", map[string]any{"action": "submitted", "pull_request": pr, "sender": bob, "review": review})[0]
	if ev.Type != core.EventPRReviewed || ev.Verdict != core.VerdictChangesRequested || ev.Subject.Kind != core.SubjectPullRequest {
		t.Fatalf("event = %+v", ev)
	}
	dis := deliver("pull_request_review", map[string]any{"action": "dismissed", "pull_request": pr, "sender": bob, "review": review})[0]
	if dis.DedupKey == ev.DedupKey || dis.Verdict != core.VerdictComment {
		t.Fatalf("a dismissal must be its own fact: %+v", dis)
	}
	c := map[string]any{"id": 11, "body": "here", "user": bob, "created_at": time.Now()}
	rc := deliver("pull_request_review_comment", map[string]any{"action": "created", "pull_request": pr, "sender": bob, "comment": c})[0]
	if rc.Type != core.EventReviewComment || rc.Comment.Body != "here" || rc.Actor.Login != "bob" {
		t.Fatalf("event = %+v", rc)
	}
	pr.State, pr.Merged = "closed", true
	cl := deliver("pull_request", map[string]any{"action": "closed", "pull_request": pr, "sender": bob})[0]
	if cl.Type != core.EventPRClosed || !cl.PullRequest.Merged || cl.PullRequest.Open {
		t.Fatalf("event = %+v", cl)
	}
}

func TestPollReportsReviewsWithWebhookIdentity(t *testing.T) {
	srv, c := setup(t)
	bob := fakegithub.Human(7, "bob")
	pr := srv.AddPullRequest(repo, "t", bob)
	srv.AddReview(repo, pr.Number, bob, "abc", "CHANGES_REQUESTED", "fix", map[string]any{"path": "a.go", "line": 1, "body": "x"})
	source := gh.NewSource("s", c, gh.SourceOptions{Repositories: []string{repo}})
	res, err := source.Poll(ctx, core.PollRequest{Container: repo, Since: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, e := range res.Events {
		keys = append(keys, e.DedupKey)
	}
	got := strings.Join(keys, " ")
	for _, want := range []string{fmt.Sprintf("github:%s:review:1", repo), fmt.Sprintf("github:%s:review_comment:1", repo)} {
		if !strings.Contains(got, want) {
			t.Errorf("%s not in %s", want, got)
		}
	}
	closed := srv.ClosePullRequest(repo, pr.Number, true)
	res, err = source.Poll(ctx, core.PollRequest{Container: repo, Since: closed.UpdatedAt.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(res.Events, func(e core.Event) bool { return e.Type == core.EventPRClosed && e.PullRequest.Merged }) {
		t.Fatalf("events = %+v", res.Events)
	}
}
