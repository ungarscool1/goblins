package jira_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers/contract"
	"github.com/ungarscool1/goblins/internal/providers/jira"
	"github.com/ungarscool1/goblins/internal/providers/jira/fakejira"
)

const token = "s3cr3t-token-value"

type clock struct{ now atomic.Int64 }

func newClock() *clock {
	c := &clock{}
	c.now.Store(time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *clock) Now() time.Time          { return time.Unix(0, c.now.Load()).UTC() }
func (c *clock) Advance(d time.Duration) { c.now.Add(int64(d)) }
func person(cloud bool, login string) fakejira.User {
	if cloud {
		return fakejira.User{AccountID: login, AccountType: "atlassian", DisplayName: login}
	}
	return fakejira.User{Name: login, Key: "key-" + login, DisplayName: login}
}

type site struct {
	srv   *fakejira.Server
	c     *jira.Client
	clock *clock
	cloud bool
}

func newSite(t *testing.T, cloud bool) *site {
	t.Helper()
	clk := newClock()
	me := person(cloud, "me")
	srv := fakejira.New(cloud, "Bearer "+token, me, clk.Now)
	t.Cleanup(srv.Close)
	srv.Grant(srv.ID(me), "ADD_COMMENTS", "BROWSE_PROJECTS")
	c, err := jira.NewClient(jira.ClientOptions{URL: srv.URL,
		Auth: jira.Bearer{Token: core.NewSecret(token)}, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return &site{srv: srv, c: c, clock: clk, cloud: cloud}
}

func (s *site) tracker(t *testing.T, write bool) *jira.Tracker {
	t.Helper()
	tr, err := jira.NewTracker("jira", s.c, jira.TrackerOptions{Projects: []string{"OPS"},
		Write: write})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestTrackerContract(t *testing.T) {
	for _, cloud := range []bool{true, false} {
		for _, write := range []bool{true, false} {
			name := map[bool]string{true: "cloud", false: "data center"}[cloud] +
				map[bool]string{true: ", writing", false: ", read-only"}[write]
			t.Run(name, func(t *testing.T) {
				contract.IssueTracker(t, func(t *testing.T) contract.TrackerHarness {
					s := newSite(t, cloud)
					s.srv.PageSize = 1 // every listing goes through paging
					return contract.TrackerHarness{Tracker: s.tracker(t, write), Container: "OPS",
						Seed: func(t *testing.T, title, body string) core.IssueRef {
							i := s.srv.AddIssue("OPS", title, body, person(cloud, "reporter"))
							return core.IssueRef{Container: "OPS", ExternalID: i.Key}
						},
						SeedComment: func(t *testing.T, ref core.IssueRef, login,
							body string) core.Comment {
							c := s.srv.AddComment(ref.ExternalID, person(cloud, login), body)
							return core.Comment{ID: c.ID}
						},
						Grant: func(t *testing.T, login string, p core.Permission) {
							perms := map[core.Permission][]string{
								core.PermissionWrite: {"EDIT_ISSUES"},
								core.PermissionAdmin: {"ADMINISTER_PROJECTS"}}[p]
							s.srv.Grant(login, append(perms, "BROWSE_PROJECTS")...)
						}}
				})
			})
		}
	}
}

// Goblins writes as a person, whose own comments must keep counting as theirs.
func TestOwnCommentsAreOnlyThoseGoblinsPosted(t *testing.T) {
	s := newSite(t, true)
	tr := s.tracker(t, true)
	i := s.srv.AddIssue("OPS", "T", "B", person(true, "reporter"))
	ref := core.IssueRef{Container: "OPS", ExternalID: i.Key}
	human := s.srv.AddComment(i.Key, person(true, "me"), "/approve-design 1")
	posted, err := tr.PostComment(context.Background(), ref, core.CommentDraft{IdempotencyKey: "k",
		Body: "## Proposed design\n\n**Approve:** `/approve-design 1`"})
	if err != nil {
		t.Fatal(err)
	}
	all, err := tr.ListComments(context.Background(), ref, time.Time{})
	if err != nil || len(all) != 2 {
		t.Fatalf("comments = %+v, %v", all, err)
	}
	for _, c := range all {
		switch c.ID {
		case human.ID:
			if c.Own || c.Author.Type != core.ActorHuman {
				t.Errorf("the person's own comment became Goblins': %+v", c)
			}
		case posted.ID:
			if !c.Own || c.Author.Type == core.ActorHuman {
				t.Errorf("Goblins' comment looks human: %+v", c)
			}
			if !strings.HasPrefix(c.Body, jira.Prefix+"h2. Proposed design") ||
				!strings.Contains(c.Body, "{{/approve-design 1}}") {
				t.Errorf("body = %q", c.Body)
			}
		}
	}
}

func TestErrorsSayWhatAPersonMustDo(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		setup func(s *site)
		write bool
		kind  core.ErrorKind
		words string
	}{
		{"read-only", func(*site) {}, false, core.KindUnsupported, "tracker.comment.write"},
		{"no comment permission", func(s *site) { s.srv.ResetPermissions(s.srv.ID(s.srv.Me)) },
			true,
			core.KindForbidden, "not allowed"},
		{"expired credentials", func(s *site) { s.srv.Expire() }, true, core.KindAuthRequired,
			"expired or were revoked"},
		{"API disabled", func(s *site) { s.srv.DisableAPI() }, true, core.KindUnsupported,
			"does not allow"},
		{"rate limited", func(s *site) { s.srv.RateLimit(5) }, true, core.KindRateLimited,
			"rate limiting"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSite(t, true)
			i := s.srv.AddIssue("OPS", "T", "B", person(true, "reporter"))
			tr := s.tracker(t, c.write)
			if _, err := tr.ListComments(ctx, core.IssueRef{Container: "OPS", ExternalID: i.Key},
				time.Time{}); err != nil {
				t.Fatal(err) // warms the identity before the failure
			}
			c.setup(s)
			_, err := tr.PostComment(ctx, core.IssueRef{Container: "OPS", ExternalID: i.Key},
				core.CommentDraft{IdempotencyKey: "k", Body: "hi"})
			if !core.IsKind(err, c.kind) || !strings.Contains(err.Error(), c.words) {
				t.Fatalf("err = %v, want %s saying %q", err, c.kind, c.words)
			}
			if strings.Contains(err.Error(), token) {
				t.Fatal("the error carries the token")
			}
			if c.kind == core.KindRateLimited &&
				!core.RetryAtOf(err).Equal(s.clock.Now().Add(30*time.Second)) {
				t.Fatalf("retry at %v", core.RetryAtOf(err))
			}
		})
	}
}

func TestProjectsOutsideTheAllowListAreRefused(t *testing.T) {
	s := newSite(t, true)
	tr := s.tracker(t, true)
	other := s.srv.AddIssue("SEC", "secret", "x", person(true, "reporter"))
	ctx := context.Background()
	if _, err := tr.GetIssue(ctx, core.IssueRef{Container: "SEC",
		ExternalID: other.Key}); !core.IsKind(err, core.KindForbidden) {
		t.Fatalf("other project: %v", err)
	}
	// Asking for another project's issue under an allowed project finds nothing.
	if _, err := tr.GetIssue(ctx, core.IssueRef{Container: "OPS",
		ExternalID: other.Key}); !core.IsKind(err, core.KindNotFound) {
		t.Fatalf("cross-project key: %v", err)
	}
	if _, err := tr.ListIssues(ctx,
		core.IssueQuery{Container: `OPS" OR project = "SEC`}); err == nil {
		t.Fatal("a container reached JQL")
	}
}
