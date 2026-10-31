// Package contract holds the behavioral test suites every adapter must pass.
// A new adapter gets its conformance tests by calling the suite for its kind
// with a constructor, without any edit to workflow code.
package contract

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

func checkInfo(t *testing.T, p core.Provider, kind core.ProviderKind) {
	t.Helper()
	i := p.Info()
	if i.Kind != kind || i.Type == "" || i.Name == "" {
		t.Fatalf("incomplete provider info: %+v", i)
	}
	if i.ContractVersion != core.ContractVersion {
		t.Fatalf("contract version %d, want %d", i.ContractVersion, core.ContractVersion)
	}
}

func wantKind(t *testing.T, err error, kind core.ErrorKind) {
	t.Helper()
	if !core.IsKind(err, kind) {
		t.Fatalf("error kind = %q (%v), want %q", core.KindOf(err), err, kind)
	}
}

// TrackerHarness supplies a tracker under test and a way to seed it.
type TrackerHarness struct {
	Tracker   core.IssueTracker
	Container string
	// Seed creates an issue authored by someone other than the tracker's bot.
	Seed func(t *testing.T, title, body string) core.IssueRef
	// SeedComment adds a comment by a human with the given login.
	SeedComment func(t *testing.T, ref core.IssueRef, login, body string) core.Comment
	// Grant gives a login a permission.
	Grant func(t *testing.T, login string, p core.Permission)
}

// IssueTracker runs the tracker conformance suite.
func IssueTracker(t *testing.T, build func(t *testing.T) TrackerHarness) {
	ctx := context.Background()
	t.Run("info", func(t *testing.T) { checkInfo(t, build(t).Tracker, core.ProviderTracker) })

	t.Run("get and list", func(t *testing.T) {
		h := build(t)
		ref := h.Seed(t, "Title", "Body")
		iss, err := h.Tracker.GetIssue(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		if iss.Title != "Title" || iss.Body != "Body" || iss.Ref != ref || !iss.Open {
			t.Fatalf("issue not canonical: %+v", iss)
		}
		list, err := h.Tracker.ListIssues(ctx, core.IssueQuery{Container: h.Container, OpenOnly: true})
		if err != nil || len(list) != 1 {
			t.Fatalf("list = %v, %v", list, err)
		}
		_, err = h.Tracker.GetIssue(ctx, core.IssueRef{Container: h.Container, ExternalID: "does-not-exist"})
		wantKind(t, err, core.KindNotFound)
	})

	t.Run("comments are idempotent per key", func(t *testing.T) {
		h := build(t)
		ref := h.Seed(t, "T", "B")
		if !h.Tracker.Info().Has(core.CapTrackerCommentWrite) {
			t.Skip("no comment.write")
		}
		a, err := h.Tracker.PostComment(ctx, ref, core.CommentDraft{IdempotencyKey: "k1", Body: "hello"})
		if err != nil {
			t.Fatal(err)
		}
		b, err := h.Tracker.PostComment(ctx, ref, core.CommentDraft{IdempotencyKey: "k1", Body: "hello"})
		if err != nil {
			t.Fatal(err)
		}
		if a.ID != b.ID {
			t.Fatalf("retry created a second comment: %s vs %s", a.ID, b.ID)
		}
		if _, err := h.Tracker.PostComment(ctx, ref, core.CommentDraft{IdempotencyKey: "k2", Body: "other"}); err != nil {
			t.Fatal(err)
		}
		all, err := h.Tracker.ListComments(ctx, ref, time.Time{})
		if err != nil || len(all) != 2 {
			t.Fatalf("comments = %d, %v", len(all), err)
		}
		if all[0].Author.Type == core.ActorHuman {
			t.Fatal("tracker's own comments must not look human")
		}
	})

	t.Run("seeded comments keep their human author", func(t *testing.T) {
		h := build(t)
		ref := h.Seed(t, "T", "B")
		h.SeedComment(t, ref, "alice", "hi")
		all, err := h.Tracker.ListComments(ctx, ref, time.Time{})
		if err != nil || len(all) != 1 || all[0].Author.Login != "alice" || all[0].Author.Type != core.ActorHuman {
			t.Fatalf("comments = %+v, %v", all, err)
		}
	})

	t.Run("permissions fail closed", func(t *testing.T) {
		h := build(t)
		ref := h.Seed(t, "T", "B")
		if !h.Tracker.Info().Has(core.CapTrackerPermissions) {
			t.Skip("no permissions")
		}
		p, err := h.Tracker.ActorPermission(ctx, ref, core.Actor{Login: "stranger", Type: core.ActorHuman})
		if err == nil && p >= core.PermissionWrite {
			t.Fatalf("unknown actor got %s", p)
		}
		h.Grant(t, "maintainer", core.PermissionWrite)
		p, err = h.Tracker.ActorPermission(ctx, ref, core.Actor{Login: "maintainer", Type: core.ActorHuman})
		if err != nil || p != core.PermissionWrite {
			t.Fatalf("maintainer = %s, %v", p, err)
		}
	})

	t.Run("missing capabilities refuse explicitly", func(t *testing.T) {
		h := build(t)
		ref := h.Seed(t, "T", "B")
		info := h.Tracker.Info()
		if !info.Has(core.CapTrackerCommentWrite) {
			_, err := h.Tracker.PostComment(ctx, ref, core.CommentDraft{Body: "x"})
			wantKind(t, err, core.KindUnsupported)
		}
		if !info.Has(core.CapTrackerPermissions) {
			_, err := h.Tracker.ActorPermission(ctx, ref, core.Actor{Login: "x"})
			wantKind(t, err, core.KindUnsupported)
		}
		if !info.Has(core.CapTrackerLabels) {
			wantKind(t, h.Tracker.SetLabels(ctx, ref, []string{"x"}, nil), core.KindUnsupported)
		}
		if !info.Has(core.CapTrackerTransitions) {
			wantKind(t, h.Tracker.Transition(ctx, ref, "closed"), core.KindUnsupported)
		}
	})
}

// SCMHarness supplies an SCM under test with one repository that has a branch.
type SCMHarness struct {
	SCM        core.SourceControl
	Repo       core.RepoRef
	BaseBranch string
	BaseSHA    string
	WorkBranch string // exists and differs from the base
}

// SourceControl runs the SCM conformance suite.
func SourceControl(t *testing.T, build func(t *testing.T) SCMHarness) {
	ctx := context.Background()
	t.Run("info", func(t *testing.T) { checkInfo(t, build(t).SCM, core.ProviderSCM) })

	t.Run("branches", func(t *testing.T) {
		h := build(t)
		sha, err := h.SCM.ResolveBranch(ctx, h.Repo, h.BaseBranch)
		if err != nil || sha != h.BaseSHA {
			t.Fatalf("resolve = %s, %v", sha, err)
		}
		_, err = h.SCM.ResolveBranch(ctx, h.Repo, "no-such-branch")
		wantKind(t, err, core.KindNotFound)
	})

	t.Run("git access never leaks through formatting", func(t *testing.T) {
		h := build(t)
		a, err := h.SCM.GitAccess(ctx, h.Repo, false)
		if err != nil {
			t.Fatal(err)
		}
		if a.RemoteURL == "" {
			t.Fatal("no remote URL")
		}
		if !a.Password.Empty() && bytes.Contains([]byte(sprint(a)), []byte(a.Password.Reveal())) {
			t.Fatal("password visible when formatted")
		}
	})

	t.Run("one pull request per head", func(t *testing.T) {
		h := build(t)
		if !h.SCM.Info().Has(core.CapSCMPullRequests) {
			_, _, err := h.SCM.EnsurePullRequest(ctx, core.PullRequestDraft{Repo: h.Repo, Head: h.WorkBranch, Base: h.BaseBranch, Title: "t"})
			wantKind(t, err, core.KindUnsupported)
			return
		}
		d := core.PullRequestDraft{IdempotencyKey: "pr", Repo: h.Repo, Head: h.WorkBranch, Base: h.BaseBranch, Title: "first", Body: "b", Draft: h.SCM.Info().Has(core.CapSCMDraft)}
		pr, created, err := h.SCM.EnsurePullRequest(ctx, d)
		if err != nil || !created {
			t.Fatalf("create = %v, %v", created, err)
		}
		if pr.Number == "" || pr.Head != h.WorkBranch || !pr.Open {
			t.Fatalf("pr not canonical: %+v", pr)
		}
		d.Title = "second"
		again, created, err := h.SCM.EnsurePullRequest(ctx, d)
		if err != nil || created || again.Number != pr.Number {
			t.Fatalf("update = %+v created=%v, %v", again, created, err)
		}
		got, err := h.SCM.GetPullRequest(ctx, h.Repo, pr.Number)
		if err != nil || got.Number != pr.Number {
			t.Fatalf("get = %+v, %v", got, err)
		}
		if err := h.SCM.PostPullRequestComment(ctx, h.Repo, pr.Number, core.CommentDraft{IdempotencyKey: "c", Body: "note"}); err != nil {
			t.Fatal(err)
		}
		if err := h.SCM.PostReview(ctx, h.Repo, pr.Number, core.ReviewDraft{IdempotencyKey: "r", Verdict: core.VerdictComment, Body: "ok"}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.SCM.ReviewThreads(ctx, h.Repo, pr.Number); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("status checks capability", func(t *testing.T) {
		h := build(t)
		_, err := h.SCM.StatusChecks(ctx, h.Repo, h.BaseSHA)
		if h.SCM.Info().Has(core.CapSCMStatusChecks) {
			if err != nil {
				t.Fatal(err)
			}
		} else {
			wantKind(t, err, core.KindUnsupported)
		}
	})
}

// EventSourceHarness supplies an event source and valid/invalid deliveries.
type EventSourceHarness struct {
	Container string
	Source    core.EventSource
	Valid     core.WebhookRequest
	Forged    core.WebhookRequest
	// Emit makes a signal available for the next Poll, when polling is supported.
	Emit func(t *testing.T)
}

// EventSource runs the event source conformance suite.
func EventSource(t *testing.T, build func(t *testing.T) EventSourceHarness) {
	ctx := context.Background()
	t.Run("info", func(t *testing.T) { checkInfo(t, build(t).Source, core.ProviderEventSource) })

	t.Run("webhook authenticates and dedup key is stable", func(t *testing.T) {
		h := build(t)
		if !h.Source.Info().Has(core.CapEventsWebhook) {
			_, err := h.Source.HandleWebhook(ctx, h.Valid)
			wantKind(t, err, core.KindUnsupported)
			return
		}
		_, err := h.Source.HandleWebhook(ctx, h.Forged)
		wantKind(t, err, core.KindUnauthorized)
		a, err := h.Source.HandleWebhook(ctx, h.Valid)
		if err != nil || len(a) == 0 {
			t.Fatalf("valid delivery = %v, %v", a, err)
		}
		b, err := h.Source.HandleWebhook(ctx, h.Valid)
		if err != nil || len(b) != len(a) {
			t.Fatalf("redelivery = %v, %v", b, err)
		}
		for i := range a {
			if a[i].DedupKey == "" || a[i].DedupKey != b[i].DedupKey {
				t.Fatalf("redelivery has another identity: %q vs %q", a[i].DedupKey, b[i].DedupKey)
			}
		}
	})

	t.Run("poll cursor advances", func(t *testing.T) {
		h := build(t)
		if !h.Source.Info().Has(core.CapEventsPoll) {
			_, err := h.Source.Poll(ctx, core.PollRequest{Container: h.Container})
			wantKind(t, err, core.KindUnsupported)
			return
		}
		h.Emit(t)
		first, err := h.Source.Poll(ctx, core.PollRequest{Container: h.Container, Since: time.Now().Add(-time.Hour)})
		if err != nil || len(first.Events) == 0 || first.NextCursor == "" {
			t.Fatalf("poll = %+v, %v", first, err)
		}
		// A source may re-report events after the cursor: the dedup key keeps
		// that harmless, so only the call's success is part of the contract.
		if _, err := h.Source.Poll(ctx, core.PollRequest{Container: h.Container, Cursor: first.NextCursor}); err != nil {
			t.Fatal(err)
		}
	})
}

// AgentProvider runs the agent conformance suite. build returns an agent
// scripted to succeed with a known output on each run.
func AgentProvider(t *testing.T, build func(t *testing.T) (core.AgentProvider, core.ExecutionRuntime, string)) {
	ctx := context.Background()
	t.Run("info", func(t *testing.T) {
		a, _, _ := build(t)
		checkInfo(t, a, core.ProviderAgent)
	})
	t.Run("run streams session then result and can resume", func(t *testing.T) {
		a, rt, want := build(t)
		var evs []core.AgentEvent
		res, err := a.Run(ctx, core.AgentRequest{IdempotencyKey: "a1", Role: core.RolePlanner, Prompt: "p", Runtime: rt}, func(e core.AgentEvent) { evs = append(evs, e) })
		if err != nil {
			t.Fatal(err)
		}
		if res.SessionID == "" || res.Output != want {
			t.Fatalf("result = %+v", res)
		}
		if len(evs) == 0 || evs[0].Kind != core.AgentEventSession {
			t.Fatalf("session must come first: %+v", evs)
		}
		if a.Info().Has(core.CapAgentResume) {
			again, err := a.Run(ctx, core.AgentRequest{IdempotencyKey: "a2", Role: core.RolePlanner, Prompt: "p2", ResumeSessionID: res.SessionID, Runtime: rt}, func(core.AgentEvent) {})
			if err != nil || again.SessionID != res.SessionID {
				t.Fatalf("resume = %+v, %v", again, err)
			}
		}
	})
	t.Run("auth and models", func(t *testing.T) {
		a, rt, _ := build(t)
		st, err := a.AuthStatus(ctx, rt)
		if err != nil || st.State == "" {
			t.Fatalf("auth = %+v, %v", st, err)
		}
		if _, err := a.Models(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// RuntimeHarness supplies a runtime whose workloads follow a convention:
// the command `["ok", text]` prints text and exits 0, `["fail"]` exits non-zero
// and `["hang"]` runs until cancelled.
type RuntimeHarness struct {
	Runtime core.ExecutionRuntime
	Image   string
	// Translate maps the conventional command onto the runtime's real commands.
	Translate func(cmd ...string) []string
}

// ExecutionRuntime runs the runtime conformance suite.
func ExecutionRuntime(t *testing.T, build func(t *testing.T) RuntimeHarness) {
	ctx := context.Background()
	spec := func(h RuntimeHarness, name string, cmd ...string) core.WorkloadSpec {
		c := cmd
		if h.Translate != nil {
			c = h.Translate(cmd...)
		}
		return core.WorkloadSpec{Name: name, Image: h.Image, Command: c, Network: core.NetworkNone, Labels: map[string]string{"goblins.test": "contract", "goblins.name": name}}
	}
	waitTerminal := func(t *testing.T, rt core.ExecutionRuntime, name string) core.WorkloadStatus {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			st, err := rt.Status(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			if st.Phase.Terminal() {
				return st
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("workload %s never finished", name)
		return core.WorkloadStatus{}
	}
	t.Run("info", func(t *testing.T) { checkInfo(t, build(t).Runtime, core.ProviderRuntime) })

	t.Run("create logs status cleanup", func(t *testing.T) {
		h := build(t)
		name := uniqueName(t, "ok")
		s := spec(h, name, "ok", "hello")
		t.Cleanup(func() { _ = h.Runtime.Cleanup(ctx, name) })
		if _, err := h.Runtime.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Runtime.Create(ctx, s); err != nil {
			t.Fatalf("create must be idempotent: %v", err)
		}
		st := waitTerminal(t, h.Runtime, name)
		if st.Phase != core.WorkloadSucceeded || st.ExitCode != 0 {
			t.Fatalf("status = %+v", st)
		}
		var out, errb bytes.Buffer
		if err := h.Runtime.Logs(ctx, name, true, &out, &errb); err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(out.Bytes(), []byte("hello")) {
			t.Fatalf("stdout = %q", out.String())
		}
		list, err := h.Runtime.List(ctx, map[string]string{"goblins.name": name})
		if err != nil || len(list) != 1 {
			t.Fatalf("list = %v, %v", list, err)
		}
		if err := h.Runtime.Cleanup(ctx, name); err != nil {
			t.Fatal(err)
		}
		if err := h.Runtime.Cleanup(ctx, name); err != nil {
			t.Fatalf("cleanup must be idempotent: %v", err)
		}
		_, err = h.Runtime.Status(ctx, name)
		wantKind(t, err, core.KindNotFound)
	})

	t.Run("failure is reported", func(t *testing.T) {
		h := build(t)
		name := uniqueName(t, "fail")
		t.Cleanup(func() { _ = h.Runtime.Cleanup(ctx, name) })
		if _, err := h.Runtime.Create(ctx, spec(h, name, "fail")); err != nil {
			t.Fatal(err)
		}
		st := waitTerminal(t, h.Runtime, name)
		if st.Phase != core.WorkloadFailed || st.ExitCode == 0 {
			t.Fatalf("status = %+v", st)
		}
	})

	t.Run("cancel stops a running workload", func(t *testing.T) {
		h := build(t)
		name := uniqueName(t, "hang")
		t.Cleanup(func() { _ = h.Runtime.Cleanup(ctx, name) })
		if _, err := h.Runtime.Create(ctx, spec(h, name, "hang")); err != nil {
			t.Fatal(err)
		}
		if err := h.Runtime.Cancel(ctx, name); err != nil {
			t.Fatal(err)
		}
		if st := waitTerminal(t, h.Runtime, name); st.Phase == core.WorkloadSucceeded {
			t.Fatalf("cancelled workload succeeded: %+v", st)
		}
		if err := h.Runtime.Cancel(ctx, name); err != nil {
			t.Fatalf("cancel after end must be a no-op: %v", err)
		}
	})

	t.Run("unsupported features are refused", func(t *testing.T) {
		h := build(t)
		info := h.Runtime.Info()
		for cap, sec := range map[core.Capability]core.SecurityProfile{
			core.CapRuntimePrivileged:   {Privileged: true},
			core.CapRuntimeReadOnlyRoot: {ReadOnlyRoot: true},
			core.CapRuntimeGPU:          {GPU: true},
			core.CapRuntimeKVM:          {KVM: true},
		} {
			if info.Has(cap) {
				continue
			}
			name := uniqueName(t, "denied")
			s := spec(h, name, "ok", "x")
			s.Security = sec
			_, err := h.Runtime.Create(ctx, s)
			wantKind(t, err, core.KindUnsupported)
			if _, serr := h.Runtime.Status(ctx, name); !core.IsKind(serr, core.KindNotFound) {
				t.Fatalf("refused workload exists: %v", serr)
			}
		}
	})

	t.Run("volumes", func(t *testing.T) {
		h := build(t)
		if !h.Runtime.Info().Has(core.CapRuntimeVolumes) {
			wantKind(t, h.Runtime.EnsureVolume(ctx, "v", nil), core.KindUnsupported)
			return
		}
		name := uniqueName(t, "vol")
		for range 2 {
			if err := h.Runtime.EnsureVolume(ctx, name, map[string]string{"goblins.test": "contract"}); err != nil {
				t.Fatal(err)
			}
		}
		for range 2 {
			if err := h.Runtime.RemoveVolume(ctx, name); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// Notification checks idempotent delivery does not fail on retries.
func Notification(t *testing.T, n core.NotificationProvider) {
	t.Helper()
	checkInfo(t, n, core.ProviderNotification)
	note := core.Notification{IdempotencyKey: "n1", Severity: core.SeverityInfo, Title: "t", Body: "b"}
	for range 2 {
		if err := n.Notify(context.Background(), note); err != nil {
			t.Fatal(err)
		}
	}
}

// Validation checks a report has a known status.
func Validation(t *testing.T, v core.ValidationProvider, rt core.ExecutionRuntime) {
	t.Helper()
	checkInfo(t, v, core.ProviderValidation)
	rep, err := v.Validate(context.Background(), core.ValidationRequest{IdempotencyKey: "v1", Runtime: rt})
	if err != nil {
		t.Fatal(err)
	}
	switch rep.Status {
	case core.ValidationPassed, core.ValidationFailed, core.ValidationError:
	default:
		t.Fatalf("unknown status %q", rep.Status)
	}
}

// Review checks a verdict is present.
func Review(t *testing.T, r core.ReviewProvider) {
	t.Helper()
	checkInfo(t, r, core.ProviderReview)
	res, err := r.Review(context.Background(), core.ReviewRequest{IdempotencyKey: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict == "" {
		t.Fatal("empty verdict")
	}
}
