package jira_test

import (
	"context"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers/jira"
)

func TestPollReportsWhoDidWhat(t *testing.T) {
	for _, cloud := range []bool{true, false} {
		t.Run(map[bool]string{true: "cloud", false: "data center"}[cloud], func(t *testing.T) {
			s := newSite(t, cloud)
			s.srv.PageSize = 2
			src, err := jira.NewSource("jira", s.c, []string{"OPS"})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			start := s.clock.Now()
			reporter, lead := person(cloud, "reporter"), person(cloud, "lead")
			var keys []string
			for range 3 {
				keys = append(keys, s.srv.AddIssue("OPS", "T", "B", reporter).Key)
				s.clock.Advance(time.Second)
			}
			s.srv.AddIssue("SEC", "elsewhere", "x", reporter)
			s.srv.Change(keys[0], lead, "labels", "", "goblins")
			c := s.srv.AddComment(keys[0], lead, "/approve-design 1")
			s.clock.Advance(time.Minute)

			res, err := src.Poll(ctx, core.PollRequest{Container: "OPS", Since: start})
			if err != nil {
				t.Fatal(err)
			}
			byType := map[core.EventType][]core.Event{}
			for _, e := range res.Events {
				byType[e.Type] = append(byType[e.Type], e)
				if e.Subject.Container != "OPS" {
					t.Errorf("event from another project: %+v", e.Subject)
				}
			}
			if len(byType[core.EventIssueOpened]) != 3 {
				t.Fatalf("opened = %d of %d events", len(byType[core.EventIssueOpened]),
					len(res.Events))
			}
			lab := byType[core.EventIssueLabeled]
			if len(lab) != 1 || lab[0].Label != "goblins" || lab[0].Actor.Login != "lead" {
				t.Fatalf("labeled = %+v", lab)
			}
			com := byType[core.EventCommentCreated]
			if len(com) != 1 || com[0].Comment.ID != c.ID || com[0].Actor.Login != "lead" {
				t.Fatalf("comments = %+v", com)
			}

			// The same facts polled again collapse onto the same keys.
			again, err := src.Poll(ctx, core.PollRequest{Container: "OPS", Since: start})
			if err != nil {
				t.Fatal(err)
			}
			if keysOf(again.Events) != keysOf(res.Events) {
				t.Fatal("a second poll reported different facts")
			}

			// From the cursor, only what changed afterwards comes back.
			s.srv.EditComment(keys[0], c.ID, "/approve-design 2")
			s.srv.Change(keys[1], lead, "description", "B", "B, revised")
			s.srv.Change(keys[2], lead, "status", "To Do", "Done")
			s.clock.Advance(time.Minute)
			next, err := src.Poll(ctx, core.PollRequest{Container: "OPS", Cursor: res.NextCursor})
			if err != nil {
				t.Fatal(err)
			}
			got := map[core.EventType]int{}
			for _, e := range next.Events {
				got[e.Type]++
				if e.Type == core.EventIssueEdited && e.Actor.Login != "lead" {
					t.Errorf("an edit was attributed to %s", e.Actor.Login)
				}
			}
			if got[core.EventCommentEdited] != 1 || got[core.EventIssueEdited] != 1 ||
				got[core.EventIssueClosed] != 1 || got[core.EventCommentCreated] != 0 {
				t.Fatalf("after the cursor: %v", got)
			}
		})
	}
}

func keysOf(evs []core.Event) string {
	var s string
	for _, e := range evs {
		s += e.DedupKey + "\n"
	}
	return s
}

func TestPollRefusesOtherProjectsAndWebhooks(t *testing.T) {
	s := newSite(t, true)
	src, _ := jira.NewSource("jira", s.c, []string{"OPS"})
	if _, err := src.Poll(context.Background(),
		core.PollRequest{Container: "SEC"}); !core.IsKind(err, core.KindForbidden) {
		t.Fatalf("poll of another project: %v", err)
	}
	if _, err := src.HandleWebhook(context.Background(), core.WebhookRequest{}); !core.IsKind(err,
		core.KindUnsupported) {
		t.Fatalf("webhook: %v", err)
	}
	if src.Info().Has(core.CapEventsWebhook) || !src.Info().Has(core.CapEventsPoll) {
		t.Fatal("wrong capabilities")
	}
}

func TestPollWaitsOutARateLimit(t *testing.T) {
	s := newSite(t, true)
	src, _ := jira.NewSource("jira", s.c, []string{"OPS"})
	s.srv.RateLimit(1)
	_, err := src.Poll(context.Background(), core.PollRequest{Container: "OPS",
		Since: s.clock.Now()})
	if !core.IsKind(err, core.KindRateLimited) || core.RetryAtOf(err).IsZero() {
		t.Fatalf("err = %v", err)
	}
}
