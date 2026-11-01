// Package fake provides in-memory adapters for every provider kind. They power
// mock mode and tests, and they are held to the same contract suite as real
// adapters. All state lives in a World so adapters can observe one another.
package fake

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers"
)

// World is the shared state behind a set of fake adapters.
type World struct {
	mu         sync.Mutex
	Now        func() time.Time
	issues     map[string]*core.Issue
	comments   map[string][]core.Comment
	byKey      map[string]core.Comment
	perms      map[string]core.Permission
	branches   map[string]string
	prs        map[string]*core.PullRequest
	prComments map[string][]string
	reviews    map[string][]core.ReviewDraft
	checks     map[string][]core.StatusCheck
	notes      []core.Notification
	noteKeys   map[string]bool
	seq        int
	Bot        core.Actor
}

// NewWorld creates an empty world with a fixed bot identity.
func NewWorld() *World {
	return &World{
		Now:        time.Now,
		issues:     map[string]*core.Issue{},
		comments:   map[string][]core.Comment{},
		byKey:      map[string]core.Comment{},
		perms:      map[string]core.Permission{},
		branches:   map[string]string{},
		prs:        map[string]*core.PullRequest{},
		prComments: map[string][]string{},
		reviews:    map[string][]core.ReviewDraft{},
		checks:     map[string][]core.StatusCheck{},
		noteKeys:   map[string]bool{},
		Bot:        core.Actor{ID: "bot-1", Login: "goblins-bot", Type: core.ActorBot},
	}
}

func refKey(r core.IssueRef) string { return r.Container + "#" + r.ExternalID }

func (w *World) next() string {
	w.seq++
	return strconv.Itoa(w.seq)
}

// AddIssue seeds an issue and returns it.
func (w *World) AddIssue(container, title, body string, author core.Actor) core.Issue {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.Now()
	iss := core.Issue{
		Ref: core.IssueRef{Container: container, ExternalID: w.next()}, Title: title, Body: body,
		Open: true, Author: author, CreatedAt: now, UpdatedAt: now,
	}
	w.issues[refKey(iss.Ref)] = &iss
	return iss
}

// EditIssue changes an issue's body.
func (w *World) EditIssue(ref core.IssueRef, body string) core.Issue {
	w.mu.Lock()
	defer w.mu.Unlock()
	iss := w.issues[refKey(ref)]
	iss.Body = body
	iss.UpdatedAt = w.Now()
	return *iss
}

// AddComment seeds a comment as if an actor had written it on the tracker.
func (w *World) AddComment(ref core.IssueRef, author core.Actor, body string) core.Comment {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.addCommentLocked(ref, author, body)
}

func (w *World) addCommentLocked(ref core.IssueRef, author core.Actor, body string) core.Comment {
	now := w.Now()
	c := core.Comment{ID: w.next(), Issue: ref, Author: author, Body: body, CreatedAt: now, UpdatedAt: now}
	w.comments[refKey(ref)] = append(w.comments[refKey(ref)], c)
	return c
}

// Comments returns the conversation of an issue.
func (w *World) Comments(ref core.IssueRef) []core.Comment {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.comments[refKey(ref)])
}

// SetPermission grants an actor a permission on a container.
func (w *World) SetPermission(container, login string, p core.Permission) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.perms[container+"/"+login] = p
}

// SetBranch seeds a branch head.
func (w *World) SetBranch(repo, branch, sha string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.branches[repo+"@"+branch] = sha
}

// PullRequests lists all pull requests.
func (w *World) PullRequests() []core.PullRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]core.PullRequest, 0, len(w.prs))
	for _, pr := range w.prs {
		out = append(out, *pr)
	}
	return out
}

// Notifications lists delivered notifications.
func (w *World) Notifications() []core.Notification {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.notes)
}

// Without returns caps with the listed ones removed.
func Without(caps []core.Capability, remove ...core.Capability) []core.Capability {
	return slices.DeleteFunc(slices.Clone(caps), func(c core.Capability) bool { return slices.Contains(remove, c) })
}

func info(kind core.ProviderKind, name string, caps []core.Capability) core.ProviderInfo {
	return core.ProviderInfo{Kind: kind, Type: "fake", Name: name, ContractVersion: core.ContractVersion, Capabilities: caps}
}

// Tracker is a fake IssueTracker.
type Tracker struct {
	W    *World
	Name string
	Caps []core.Capability
}

// AllTrackerCaps lists every tracker capability.
var AllTrackerCaps = []core.Capability{core.CapTrackerCommentWrite, core.CapTrackerLabels, core.CapTrackerTransitions, core.CapTrackerPermissions}

// NewTracker returns a fully capable fake tracker.
func NewTracker(w *World, name string) *Tracker {
	return &Tracker{W: w, Name: name, Caps: AllTrackerCaps}
}

func (t *Tracker) Info() core.ProviderInfo { return info(core.ProviderTracker, t.Name, t.Caps) }

func (t *Tracker) GetIssue(_ context.Context, ref core.IssueRef) (core.Issue, error) {
	t.W.mu.Lock()
	defer t.W.mu.Unlock()
	if iss, ok := t.W.issues[refKey(ref)]; ok {
		return *iss, nil
	}
	return core.Issue{}, core.Errorf(core.KindNotFound, "fake.get_issue", "issue %s not found", refKey(ref))
}

func (t *Tracker) ListIssues(_ context.Context, q core.IssueQuery) ([]core.Issue, error) {
	t.W.mu.Lock()
	defer t.W.mu.Unlock()
	out := []core.Issue{}
	for _, iss := range t.W.issues {
		if (q.Container == "" || q.Container == iss.Ref.Container) && (!q.OpenOnly || iss.Open) && !iss.UpdatedAt.Before(q.UpdatedSince) {
			out = append(out, *iss)
		}
	}
	slices.SortFunc(out, func(a, b core.Issue) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

func (t *Tracker) ListComments(_ context.Context, ref core.IssueRef, since time.Time) ([]core.Comment, error) {
	t.W.mu.Lock()
	defer t.W.mu.Unlock()
	if _, ok := t.W.issues[refKey(ref)]; !ok {
		return nil, core.Errorf(core.KindNotFound, "fake.list_comments", "issue %s not found", refKey(ref))
	}
	out := []core.Comment{}
	for _, c := range t.W.comments[refKey(ref)] {
		if !c.UpdatedAt.Before(since) {
			c.Own = c.Author.ID == t.W.Bot.ID
			out = append(out, c)
		}
	}
	return out, nil
}

func (t *Tracker) PostComment(_ context.Context, ref core.IssueRef, d core.CommentDraft) (core.Comment, error) {
	if !t.Info().Has(core.CapTrackerCommentWrite) {
		return core.Comment{}, core.Unsupported(t.Info(), core.CapTrackerCommentWrite)
	}
	t.W.mu.Lock()
	defer t.W.mu.Unlock()
	if _, ok := t.W.issues[refKey(ref)]; !ok {
		return core.Comment{}, core.Errorf(core.KindNotFound, "fake.post_comment", "issue %s not found", refKey(ref))
	}
	key := refKey(ref) + "|" + d.IdempotencyKey
	if d.IdempotencyKey != "" {
		if c, ok := t.W.byKey[key]; ok {
			return c, nil
		}
	}
	c := t.W.addCommentLocked(ref, t.W.Bot, d.Body)
	if d.IdempotencyKey != "" {
		t.W.byKey[key] = c
	}
	return c, nil
}

func (t *Tracker) ActorPermission(_ context.Context, ref core.IssueRef, a core.Actor) (core.Permission, error) {
	if !t.Info().Has(core.CapTrackerPermissions) {
		return core.PermissionNone, core.Unsupported(t.Info(), core.CapTrackerPermissions)
	}
	t.W.mu.Lock()
	defer t.W.mu.Unlock()
	return t.W.perms[ref.Container+"/"+a.Login], nil
}

func (t *Tracker) SetLabels(_ context.Context, ref core.IssueRef, add, remove []string) error {
	if !t.Info().Has(core.CapTrackerLabels) {
		return core.Unsupported(t.Info(), core.CapTrackerLabels)
	}
	t.W.mu.Lock()
	defer t.W.mu.Unlock()
	iss, ok := t.W.issues[refKey(ref)]
	if !ok {
		return core.Errorf(core.KindNotFound, "fake.set_labels", "issue %s not found", refKey(ref))
	}
	iss.Labels = slices.DeleteFunc(iss.Labels, func(l string) bool { return slices.Contains(remove, l) })
	for _, l := range add {
		if !slices.Contains(iss.Labels, l) {
			iss.Labels = append(iss.Labels, l)
		}
	}
	return nil
}

func (t *Tracker) Transition(_ context.Context, ref core.IssueRef, status string) error {
	if !t.Info().Has(core.CapTrackerTransitions) {
		return core.Unsupported(t.Info(), core.CapTrackerTransitions)
	}
	t.W.mu.Lock()
	defer t.W.mu.Unlock()
	iss, ok := t.W.issues[refKey(ref)]
	if !ok {
		return core.Errorf(core.KindNotFound, "fake.transition", "issue %s not found", refKey(ref))
	}
	iss.Open = status != "closed"
	return nil
}

// SCM is a fake SourceControl.
type SCM struct {
	W    *World
	Name string
	Caps []core.Capability
}

// AllSCMCaps lists every SCM capability.
var AllSCMCaps = []core.Capability{core.CapSCMPullRequests, core.CapSCMDraft, core.CapSCMInlineReview, core.CapSCMStatusChecks}

// NewSCM returns a fully capable fake SCM.
func NewSCM(w *World, name string) *SCM { return &SCM{W: w, Name: name, Caps: AllSCMCaps} }

func (s *SCM) Info() core.ProviderInfo { return info(core.ProviderSCM, s.Name, s.Caps) }

func (s *SCM) GitAccess(_ context.Context, repo core.RepoRef, _ bool) (core.GitAccess, error) {
	return core.GitAccess{RemoteURL: "fake://" + repo.Slug, Username: "x", Password: core.NewSecret("fake-password"), ExpiresAt: s.W.Now().Add(time.Hour)}, nil
}

func (s *SCM) ResolveBranch(_ context.Context, repo core.RepoRef, branch string) (string, error) {
	s.W.mu.Lock()
	defer s.W.mu.Unlock()
	if sha, ok := s.W.branches[repo.Slug+"@"+branch]; ok {
		return sha, nil
	}
	return "", core.Errorf(core.KindNotFound, "fake.resolve_branch", "branch %s not found", branch)
}

func (s *SCM) GetCommit(_ context.Context, _ core.RepoRef, sha string) (core.Commit, error) {
	return core.Commit{SHA: sha, Message: "fake commit " + sha}, nil
}

func (s *SCM) EnsurePullRequest(_ context.Context, d core.PullRequestDraft) (core.PullRequest, bool, error) {
	if !s.Info().Has(core.CapSCMPullRequests) {
		return core.PullRequest{}, false, core.Unsupported(s.Info(), core.CapSCMPullRequests)
	}
	if d.Draft && !s.Info().Has(core.CapSCMDraft) {
		return core.PullRequest{}, false, core.Unsupported(s.Info(), core.CapSCMDraft)
	}
	s.W.mu.Lock()
	defer s.W.mu.Unlock()
	for _, pr := range s.W.prs {
		if pr.Repo == d.Repo && pr.Head == d.Head && pr.Open {
			pr.Title = d.Title
			pr.HeadSHA = s.W.branches[d.Repo.Slug+"@"+d.Head]
			return *pr, false, nil
		}
	}
	pr := &core.PullRequest{Repo: d.Repo, Number: s.W.next(), Title: d.Title, Head: d.Head, Base: d.Base, Draft: d.Draft, Open: true,
		HeadSHA: s.W.branches[d.Repo.Slug+"@"+d.Head], Author: s.W.Bot, URL: fmt.Sprintf("fake://%s/pull", d.Repo.Slug)}
	s.W.prs[d.Repo.Slug+"#"+pr.Number] = pr
	return *pr, true, nil
}

func (s *SCM) GetPullRequest(_ context.Context, repo core.RepoRef, number string) (core.PullRequest, error) {
	s.W.mu.Lock()
	defer s.W.mu.Unlock()
	if pr, ok := s.W.prs[repo.Slug+"#"+number]; ok {
		return *pr, nil
	}
	return core.PullRequest{}, core.Errorf(core.KindNotFound, "fake.get_pr", "pull request %s not found", number)
}

// PullRequestComments lists comments posted on a pull request, as key|body.
func (w *World) PullRequestComments(repo, number string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.prComments[repo+"#"+number])
}

func (s *SCM) PostPullRequestComment(_ context.Context, repo core.RepoRef, number string, d core.CommentDraft) error {
	s.W.mu.Lock()
	defer s.W.mu.Unlock()
	k := repo.Slug + "#" + number
	if _, ok := s.W.prs[k]; !ok {
		return core.Errorf(core.KindNotFound, "fake.pr_comment", "pull request %s not found", number)
	}
	prefix := d.IdempotencyKey + "|"
	if d.IdempotencyKey != "" && slices.ContainsFunc(s.W.prComments[k], func(c string) bool { return strings.HasPrefix(c, prefix) }) {
		return nil
	}
	s.W.prComments[k] = append(s.W.prComments[k], prefix+d.Body)
	return nil
}

func (s *SCM) PostReview(_ context.Context, repo core.RepoRef, number string, d core.ReviewDraft) error {
	if len(d.Comments) > 0 && !s.Info().Has(core.CapSCMInlineReview) {
		return core.Unsupported(s.Info(), core.CapSCMInlineReview)
	}
	s.W.mu.Lock()
	defer s.W.mu.Unlock()
	k := repo.Slug + "#" + number
	if _, ok := s.W.prs[k]; !ok {
		return core.Errorf(core.KindNotFound, "fake.review", "pull request %s not found", number)
	}
	for _, r := range s.W.reviews[k] {
		if r.IdempotencyKey == d.IdempotencyKey && d.IdempotencyKey != "" {
			return nil
		}
	}
	s.W.reviews[k] = append(s.W.reviews[k], d)
	return nil
}

// ReviewsOn lists reviews posted on a pull request.
func (w *World) ReviewsOn(repo, number string) []core.ReviewDraft {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.reviews[repo+"#"+number])
}

func (s *SCM) ReviewThreads(_ context.Context, repo core.RepoRef, number string) ([]core.ReviewThread, error) {
	s.W.mu.Lock()
	defer s.W.mu.Unlock()
	out := []core.ReviewThread{}
	for i, r := range s.W.reviews[repo.Slug+"#"+number] {
		for j, c := range r.Comments {
			out = append(out, core.ReviewThread{ID: fmt.Sprintf("%d-%d", i, j), Path: c.Path, Line: c.Line, Body: c.Body, Author: s.W.Bot})
		}
	}
	return out, nil
}

func (s *SCM) StatusChecks(_ context.Context, repo core.RepoRef, sha string) ([]core.StatusCheck, error) {
	if !s.Info().Has(core.CapSCMStatusChecks) {
		return nil, core.Unsupported(s.Info(), core.CapSCMStatusChecks)
	}
	s.W.mu.Lock()
	defer s.W.mu.Unlock()
	return slices.Clone(s.W.checks[repo.Slug+"@"+sha]), nil
}

// SetChecks seeds CI results.
func (w *World) SetChecks(repo, sha string, checks ...core.StatusCheck) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.checks[repo+"@"+sha] = checks
}

// EventSource is a fake source with a scriptable queue. Webhooks are
// "authenticated" by a shared token header to mirror real adapters.
type EventSource struct {
	Name  string
	Caps  []core.Capability
	Token string
	mu    sync.Mutex
	queue []core.Event
}

// NewEventSource returns a source supporting both webhooks and polling.
func NewEventSource(name string) *EventSource {
	return &EventSource{Name: name, Caps: []core.Capability{core.CapEventsWebhook, core.CapEventsPoll}, Token: "fake-token"}
}

func (e *EventSource) Info() core.ProviderInfo { return info(core.ProviderEventSource, e.Name, e.Caps) }

// Push queues events for the next Poll.
func (e *EventSource) Push(evs ...core.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.queue = append(e.queue, evs...)
}

func (e *EventSource) HandleWebhook(_ context.Context, req core.WebhookRequest) ([]core.Event, error) {
	if !e.Info().Has(core.CapEventsWebhook) {
		return nil, core.Unsupported(e.Info(), core.CapEventsWebhook)
	}
	if req.Header("X-Fake-Token") != e.Token {
		return nil, core.Errorf(core.KindUnauthorized, "fake.webhook", "invalid delivery signature")
	}
	if len(req.Body) == 0 {
		return []core.Event{}, nil
	}
	return []core.Event{{Source: e.Name, Type: core.EventIssueOpened, DedupKey: "webhook:" + string(req.Body), OccurredAt: time.Now()}}, nil
}

func (e *EventSource) Poll(_ context.Context, req core.PollRequest) (core.PollResult, error) {
	if !e.Info().Has(core.CapEventsPoll) {
		return core.PollResult{}, core.Unsupported(e.Info(), core.CapEventsPoll)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	start := 0
	if req.Cursor != "" {
		n, err := strconv.Atoi(req.Cursor)
		if err != nil {
			return core.PollResult{}, core.Errorf(core.KindInvalid, "fake.poll", "invalid cursor")
		}
		start = n
	}
	if start > len(e.queue) {
		start = len(e.queue)
	}
	return core.PollResult{Events: slices.Clone(e.queue[start:]), NextCursor: strconv.Itoa(len(e.queue))}, nil
}

// Review is a fake ReviewProvider returning a fixed result.
type Review struct {
	Name   string
	Result core.ReviewResult
}

func (r *Review) Info() core.ProviderInfo { return info(core.ProviderReview, r.Name, nil) }

func (r *Review) Review(_ context.Context, _ core.ReviewRequest) (core.ReviewResult, error) {
	return r.Result, nil
}

// Validation is a fake ValidationProvider returning scripted reports in order
// and repeating the last one.
type Validation struct {
	Name    string
	mu      sync.Mutex
	Reports []core.ValidationReport
	Calls   []core.ValidationRequest
}

func (v *Validation) Info() core.ProviderInfo { return info(core.ProviderValidation, v.Name, nil) }

func (v *Validation) Validate(_ context.Context, req core.ValidationRequest) (core.ValidationReport, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.Calls = append(v.Calls, req)
	if len(v.Reports) == 0 {
		return core.ValidationReport{Status: core.ValidationPassed, Summary: "fake checks passed"}, nil
	}
	i := min(len(v.Calls)-1, len(v.Reports)-1)
	return v.Reports[i], nil
}

// Notifier is a fake NotificationProvider recording into the World.
type Notifier struct {
	W    *World
	Name string
}

func (n *Notifier) Info() core.ProviderInfo { return info(core.ProviderNotification, n.Name, nil) }

func (n *Notifier) Notify(_ context.Context, note core.Notification) error {
	n.W.mu.Lock()
	defer n.W.mu.Unlock()
	if n.W.noteKeys[note.IdempotencyKey] && note.IdempotencyKey != "" {
		return nil
	}
	n.W.noteKeys[note.IdempotencyKey] = true
	n.W.notes = append(n.W.notes, note)
	return nil
}

// Register adds the fake adapters to a registry. All instances created from one
// registration share a World.
func Register(r *providers.Registry, w *World) {
	r.Register(providers.KindTracker, "fake", func(name string, _ providers.Deps, _ providers.Options) (any, error) { return NewTracker(w, name), nil })
	r.Register(providers.KindSCM, "fake", func(name string, _ providers.Deps, _ providers.Options) (any, error) { return NewSCM(w, name), nil })
	r.Register(providers.KindEventSource, "fake", func(name string, _ providers.Deps, _ providers.Options) (any, error) {
		return NewEventSource(name), nil
	})
	r.Register(providers.KindAgent, "fake", func(name string, _ providers.Deps, _ providers.Options) (any, error) {
		a := NewAgent(name)
		a.Fallback = mockAgent
		return a, nil
	})
	r.Register(providers.KindRuntime, "fake", func(name string, _ providers.Deps, _ providers.Options) (any, error) {
		rt := NewRuntime(name)
		rt.Behavior = mockWorkload
		return rt, nil
	})
	r.Register(providers.KindReview, "fake", func(name string, _ providers.Deps, _ providers.Options) (any, error) {
		return &Review{Name: name, Result: core.ReviewResult{Verdict: core.VerdictApprove, Summary: "fake review approved"}}, nil
	})
	r.Register(providers.KindValidation, "fake", func(name string, _ providers.Deps, _ providers.Options) (any, error) {
		return &Validation{Name: name}, nil
	})
	r.Register(providers.KindNotification, "fake", func(name string, _ providers.Deps, _ providers.Options) (any, error) {
		return &Notifier{W: w, Name: name}, nil
	})
}

// mockAgent gives mock mode something plausible to show: a planner that proposes a
// design, and an implementer that reports success. It consumes no tokens.
func mockAgent(req core.AgentRequest) string {
	if req.Role != core.RolePlanner {
		return "Mock implementation finished."
	}
	return "```goblins-plan\n" + `{"design":{"scope":"Mock scope","architecture":"Mock architecture","alternatives":"None considered in mock mode",` +
		`"constraints":"None","implementation_plan":"Pretend to implement it","tests":"Pretend to test it","security":"None","out_of_scope":"Everything real"}}` + "\n```"
}

// mockWorkload answers the workspace operations of a mock run.
func mockWorkload(core.WorkloadSpec) WorkloadResult {
	const sha = "1111111111111111111111111111111111111111"
	const head = "2222222222222222222222222222222222222222"
	return WorkloadResult{Stdout: `GOBLINS_RESULT {"prepared":true,"base_sha":"` + sha + `","head_sha":"` + head + `","changed":true}` + "\n"}
}
