package contract_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers/contract"
	"github.com/ungarscool1/goblins/internal/providers/fake"
)

func trackerHarness(t *testing.T, caps []core.Capability) contract.TrackerHarness {
	t.Helper()
	w := fake.NewWorld()
	tr := fake.NewTracker(w, "t")
	tr.Caps = caps
	return contract.TrackerHarness{
		Tracker: tr, Container: "acme/app",
		Seed: func(_ *testing.T, title, body string) core.IssueRef {
			return w.AddIssue("acme/app", title, body, core.Actor{Login: "alice", Type: core.ActorHuman}).Ref
		},
		SeedComment: func(_ *testing.T, ref core.IssueRef, login, body string) core.Comment {
			return w.AddComment(ref, core.Actor{Login: login, Type: core.ActorHuman}, body)
		},
		Grant: func(_ *testing.T, login string, p core.Permission) { w.SetPermission("acme/app", login, p) },
	}
}

func TestFakeTracker(t *testing.T) {
	contract.IssueTracker(t, func(t *testing.T) contract.TrackerHarness { return trackerHarness(t, fake.AllTrackerCaps) })
}

func TestFakeTrackerWithoutOptionalCapabilities(t *testing.T) {
	contract.IssueTracker(t, func(t *testing.T) contract.TrackerHarness { return trackerHarness(t, nil) })
}

func TestFakeSCM(t *testing.T) {
	contract.SourceControl(t, func(*testing.T) contract.SCMHarness {
		w := fake.NewWorld()
		w.SetBranch("acme/app", "main", "aaa")
		w.SetBranch("acme/app", "work", "bbb")
		return contract.SCMHarness{SCM: fake.NewSCM(w, "s"), Repo: core.RepoRef{Slug: "acme/app"}, BaseBranch: "main", BaseSHA: "aaa", WorkBranch: "work"}
	})
}

func TestFakeSCMWithoutPullRequests(t *testing.T) {
	contract.SourceControl(t, func(*testing.T) contract.SCMHarness {
		w := fake.NewWorld()
		w.SetBranch("acme/app", "main", "aaa")
		s := fake.NewSCM(w, "s")
		s.Caps = nil
		return contract.SCMHarness{SCM: s, Repo: core.RepoRef{Slug: "acme/app"}, BaseBranch: "main", BaseSHA: "aaa", WorkBranch: "work"}
	})
}

func TestFakeEventSource(t *testing.T) {
	contract.EventSource(t, func(*testing.T) contract.EventSourceHarness {
		src := fake.NewEventSource("e")
		return contract.EventSourceHarness{
			Source: src,
			Valid:  core.WebhookRequest{Headers: map[string][]string{"x-fake-token": {"fake-token"}}, Body: []byte("payload")},
			Forged: core.WebhookRequest{Headers: map[string][]string{"x-fake-token": {"nope"}}, Body: []byte("payload")},
			Emit:   func(*testing.T) { src.Push(core.Event{DedupKey: "k", Type: core.EventIssueOpened}) },
		}
	})
}

func TestFakeAgent(t *testing.T) {
	contract.AgentProvider(t, func(*testing.T) (core.AgentProvider, core.ExecutionRuntime, string) {
		return fake.NewAgent("a", fake.AgentStep{Output: "done"}, fake.AgentStep{Output: "done"}), fake.NewRuntime("r"), "done"
	})
}

func TestAgentErrorsAreClassified(t *testing.T) {
	a := fake.NewAgent("a", fake.AgentStep{Err: core.Errorf(core.KindQuotaExhausted, "x", "limit reached")})
	_, err := a.Run(context.Background(), core.AgentRequest{}, func(core.AgentEvent) {})
	if !core.IsKind(err, core.KindQuotaExhausted) {
		t.Fatalf("err = %v", err)
	}
	if _, err := a.Run(context.Background(), core.AgentRequest{ResumeSessionID: "forgotten"}, func(core.AgentEvent) {}); !core.IsKind(err, core.KindNotFound) {
		t.Fatalf("unknown session: %v", err)
	}
	noResume := fake.NewAgent("a")
	noResume.Caps = nil
	if _, err := noResume.Run(context.Background(), core.AgentRequest{ResumeSessionID: "s"}, func(core.AgentEvent) {}); !core.IsKind(err, core.KindUnsupported) {
		t.Fatalf("resume unsupported: %v", err)
	}
}

func runtimeHarness(typ string) contract.RuntimeHarness {
	r := fake.NewRuntime("r")
	r.TypeName = typ
	r.Behavior = func(s core.WorkloadSpec) fake.WorkloadResult {
		switch s.Command[0] {
		case "ok":
			return fake.WorkloadResult{Stdout: strings.Join(s.Command[1:], " ")}
		case "hang":
			return fake.WorkloadResult{Hang: true}
		}
		return fake.WorkloadResult{Exit: 3}
	}
	return contract.RuntimeHarness{Runtime: r, Image: "any"}
}

func TestFakeRuntime(t *testing.T) {
	contract.ExecutionRuntime(t, func(*testing.T) contract.RuntimeHarness { return runtimeHarness("fake") })
}

// A second backend passes the same suite with no change to anything else.
func TestFakeRuntimeUnderAnotherName(t *testing.T) {
	contract.ExecutionRuntime(t, func(*testing.T) contract.RuntimeHarness { return runtimeHarness("fake-cluster") })
}

func TestSmallContracts(t *testing.T) {
	w := fake.NewWorld()
	contract.Notification(t, &fake.Notifier{W: w, Name: "n"})
	if got := len(w.Notifications()); got != 1 {
		t.Fatalf("notifications = %d, want 1 after retry", got)
	}
	contract.Validation(t, &fake.Validation{Name: "v"}, fake.NewRuntime("r"))
	contract.Review(t, &fake.Review{Name: "x", Result: core.ReviewResult{Verdict: core.VerdictSkipped, Summary: "no reviewer"}})
}

// listTracker is a deliberately small, independent tracker: it proves a new
// tracker only has to pass this suite. It supports reading only.
type listTracker struct {
	issues   map[string]core.Issue
	comments map[string][]core.Comment
}

func (l *listTracker) Info() core.ProviderInfo {
	return core.ProviderInfo{Kind: core.ProviderTracker, Type: "list", Name: "l", ContractVersion: core.ContractVersion}
}
func (l *listTracker) GetIssue(_ context.Context, r core.IssueRef) (core.Issue, error) {
	if i, ok := l.issues[r.ExternalID]; ok {
		return i, nil
	}
	return core.Issue{}, core.Errorf(core.KindNotFound, "list", "no issue")
}
func (l *listTracker) ListIssues(context.Context, core.IssueQuery) ([]core.Issue, error) {
	out := []core.Issue{}
	for _, i := range l.issues {
		out = append(out, i)
	}
	return out, nil
}
func (l *listTracker) ListComments(_ context.Context, r core.IssueRef, _ time.Time) ([]core.Comment, error) {
	return l.comments[r.ExternalID], nil
}
func (l *listTracker) PostComment(context.Context, core.IssueRef, core.CommentDraft) (core.Comment, error) {
	return core.Comment{}, core.Unsupported(l.Info(), core.CapTrackerCommentWrite)
}
func (l *listTracker) ActorPermission(context.Context, core.IssueRef, core.Actor) (core.Permission, error) {
	return core.PermissionNone, core.Unsupported(l.Info(), core.CapTrackerPermissions)
}
func (l *listTracker) SetLabels(context.Context, core.IssueRef, []string, []string) error {
	return core.Unsupported(l.Info(), core.CapTrackerLabels)
}
func (l *listTracker) Transition(context.Context, core.IssueRef, string) error {
	return core.Unsupported(l.Info(), core.CapTrackerTransitions)
}

func TestIndependentTrackerPassesSuite(t *testing.T) {
	contract.IssueTracker(t, func(*testing.T) contract.TrackerHarness {
		l := &listTracker{issues: map[string]core.Issue{}, comments: map[string][]core.Comment{}}
		n := 0
		return contract.TrackerHarness{
			Tracker: l, Container: "c",
			Seed: func(_ *testing.T, title, body string) core.IssueRef {
				n++
				ref := core.IssueRef{Container: "c", ExternalID: strconv.Itoa(n)}
				l.issues[ref.ExternalID] = core.Issue{Ref: ref, Title: title, Body: body, Open: true}
				return ref
			},
			SeedComment: func(_ *testing.T, ref core.IssueRef, login, body string) core.Comment {
				c := core.Comment{ID: "c", Issue: ref, Body: body, Author: core.Actor{Login: login, Type: core.ActorHuman}}
				l.comments[ref.ExternalID] = append(l.comments[ref.ExternalID], c)
				return c
			},
		}
	})
}

func TestProcessRuntime(t *testing.T) {
	contract.ExecutionRuntime(t, func(t *testing.T) contract.RuntimeHarness {
		return contract.RuntimeHarness{
			Runtime: fake.NewProcessRuntime("p", t.TempDir(), ""),
			Translate: func(cmd ...string) []string {
				switch cmd[0] {
				case "ok":
					return []string{"sh", "-c", "echo " + strings.Join(cmd[1:], " ")}
				case "hang":
					return []string{"sleep", "300"}
				}
				return []string{"sh", "-c", "exit 3"}
			},
		}
	})
}
