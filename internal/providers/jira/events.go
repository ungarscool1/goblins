package jira

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// Source reports Jira activity by polling: a webhook would need a Jira administrator, which
// Goblins does not assume. A poll reads issues updated since the cursor, with their
// changelog, which says who changed what, and their comments.
type Source struct {
	name     string
	c        *Client
	projects []string
}

// NewSource returns an event source for an allow-list of projects.
func NewSource(name string, c *Client, projects []string) (*Source, error) {
	if len(projects) == 0 {
		return nil, core.Errorf(core.KindInvalid, "jira.source", "projects allow-list is required")
	}
	for _, p := range projects {
		if !projectKey.MatchString(p) {
			return nil, core.Errorf(core.KindInvalid, "jira.source", "%q is not a project key", p)
		}
	}
	return &Source{name: name, c: c, projects: projects}, nil
}

// Info implements core.Provider.
func (s *Source) Info() core.ProviderInfo {
	return info(core.ProviderEventSource, s.name, core.CapEventsPoll)
}

// Serves implements core.ContainerScoped.
func (s *Source) Serves(container string) bool { return slices.Contains(s.projects, container) }

// HandleWebhook implements core.EventSource. Jira webhooks need an administrator, so they
// are not supported.
func (s *Source) HandleWebhook(context.Context, core.WebhookRequest) ([]core.Event, error) {
	return nil, core.Unsupported(s.Info(), core.CapEventsWebhook)
}

// Poll implements core.EventSource. The cursor is the latest update seen; the next poll asks
// from a little before it, and dedup makes the overlap harmless.
func (s *Source) Poll(ctx context.Context, req core.PollRequest) (core.PollResult, error) {
	if !s.Serves(req.Container) {
		return core.PollResult{}, core.Errorf(core.KindForbidden, "jira.poll",
			"project is not allowed")
	}
	since := req.Since
	if req.Cursor != "" {
		t, err := time.Parse(time.RFC3339Nano, req.Cursor)
		if err != nil {
			return core.PollResult{}, core.Errorf(core.KindInvalid, "jira.poll", "invalid cursor")
		}
		since = t
	}
	me, err := s.c.Myself(ctx)
	if err != nil {
		return core.PollResult{}, err
	}
	jql := fmt.Sprintf(`project = "%s" AND %s ORDER BY updated ASC`, req.Container,
		updatedSince(s.c.now(), since))
	latest, evs := since, []core.Event{}
	_, err = s.c.search(ctx, jql, "changelog", 0, func(i issue) error {
		if i.Fields.Project.Key != req.Container || !issueKey.MatchString(i.Key) {
			return nil
		}
		updated := jiraTime(i.Fields.Updated)
		if updated.Before(since) {
			return nil
		}
		evs = append(evs, issueEvents(i, since, s.c.base, s.site())...)
		ref := core.IssueRef{Container: req.Container, ExternalID: i.Key}
		if err := s.c.comments(ctx, i.Key, func(c comment) error {
			if t := jiraTime(c.Updated); !t.Before(since) {
				evs = append(evs, commentEvent(ref, c, me, s.site()))
			}
			return nil
		}); err != nil {
			return err
		}
		if updated.After(latest) {
			latest = updated
		}
		return nil
	})
	if err != nil {
		return core.PollResult{}, err
	}
	// Results come oldest first, so a listing cut short continues from its last issue.
	return core.PollResult{Events: evs, NextCursor: latest.UTC().Format(time.RFC3339Nano)}, nil
}

// site prefixes dedup keys, so two Jira sites in one installation never share a fact.
func (s *Source) site() string {
	return "jira:" + strings.TrimPrefix(strings.TrimPrefix(s.c.base, "https://"), "http://")
}

func subject(i issue) core.Subject {
	return core.Subject{Kind: core.SubjectIssue, Container: i.Fields.Project.Key, ExternalID: i.Key}
}

// issueEvents reports what happened to an issue since a moment. Edits and labels come from
// the changelog, which names who made them: the permission of that person, not the
// reporter's, decides whether they count.
func issueEvents(i issue, since time.Time, base, site string) []core.Event {
	c := i.canonical(base)
	key := site + ":issue:" + i.ID
	var evs []core.Event
	if !c.CreatedAt.Before(since) {
		evs = append(evs, core.Event{Type: core.EventIssueOpened, Subject: subject(i),
			Actor: i.reporter(), Issue: &c, OccurredAt: c.CreatedAt, DedupKey: key + ":opened"})
	}
	for _, h := range i.Changelog.Histories {
		at := jiraTime(h.Created)
		if at.Before(since) {
			continue
		}
		actor := core.Actor{Type: core.ActorUnknown}
		if h.Author != nil {
			actor = h.Author.actor()
		}
		evs = append(evs, historyEvents(i, c, h, actor, at, key)...)
	}
	return evs
}

func historyEvents(i issue, c core.Issue, h history, actor core.Actor, at time.Time,
	key string) []core.Event {
	var evs []core.Event
	edited := false
	for _, item := range h.Items {
		switch item.Field {
		case "summary", "description":
			edited = true
		case "labels":
			for _, l := range added(item.FromString, item.ToString) {
				evs = append(evs, core.Event{Type: core.EventIssueLabeled, Subject: subject(i),
					Actor: actor, Issue: &c, Label: l, OccurredAt: at,
					DedupKey: fmt.Sprintf("%s:labeled:%s:%s", key, h.ID, l)})
			}
		case "status", "resolution":
			if !i.open() {
				evs = append(evs, core.Event{Type: core.EventIssueClosed, Subject: subject(i),
					Actor: actor, Issue: &c, OccurredAt: at, DedupKey: key + ":closed:" + h.ID})
			}
		}
	}
	if edited {
		// The issue carries today's text: an edit reports the text as it is now, which is
		// what the workflow compares with what was accepted.
		evs = append(evs, core.Event{Type: core.EventIssueEdited, Subject: subject(i),
			Actor: actor, Issue: &c, OccurredAt: at, DedupKey: key + ":edited:" + h.ID})
	}
	return evs
}

// added lists the labels in to that are not in from. Jira gives both as space-separated.
func added(from, to string) []string {
	before := strings.Fields(from)
	var out []string
	for _, l := range strings.Fields(to) {
		if !slices.Contains(before, l) {
			out = append(out, l)
		}
	}
	return out
}

func commentEvent(ref core.IssueRef, c comment, me user, site string) core.Event {
	cc := c.canonical(ref, me)
	subj := core.Subject{Kind: core.SubjectIssue, Container: ref.Container,
		ExternalID: ref.ExternalID}
	ev := core.Event{Type: core.EventCommentCreated, Subject: subj, Actor: cc.Author, Comment: &cc,
		OccurredAt: cc.CreatedAt, DedupKey: site + ":comment:" + c.ID + ":created"}
	// The text at creation is gone once edited. Reporting today's text as new would let an
	// edit pass as an approval.
	if cc.UpdatedAt.After(cc.CreatedAt) {
		ev.Type, ev.OccurredAt = core.EventCommentEdited, cc.UpdatedAt
		ev.DedupKey = fmt.Sprintf("%s:comment:%s:edited:%d", site, c.ID, cc.UpdatedAt.UnixNano())
	}
	return ev
}
