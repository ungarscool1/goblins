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

// Tracker implements core.IssueTracker on GitHub issues.
type Tracker struct {
	name string
	c    *Client
}

// NewTracker returns a tracker using c.
func NewTracker(name string, c *Client) *Tracker { return &Tracker{name: name, c: c} }

// Info implements core.Provider.
func (t *Tracker) Info() core.ProviderInfo {
	return info(core.ProviderTracker, t.name, core.CapTrackerCommentWrite, core.CapTrackerLabels, core.CapTrackerTransitions, core.CapTrackerPermissions)
}

type label struct {
	Name string `json:"name"`
}

type issue struct {
	Number      int64           `json:"number"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	State       string          `json:"state"`
	HTMLURL     string          `json:"html_url"`
	User        user            `json:"user"`
	Labels      []label         `json:"labels"`
	PullRequest json.RawMessage `json:"pull_request"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

func (i issue) canonical(repo string) core.Issue {
	out := core.Issue{
		Ref: core.IssueRef{Container: repo, ExternalID: strconv.FormatInt(i.Number, 10)}, Title: i.Title, Body: i.Body, Open: i.State == "open",
		Author: actorOf(i.User), URL: i.HTMLURL, CreatedAt: i.CreatedAt, UpdatedAt: i.UpdatedAt,
	}
	for _, l := range i.Labels {
		out.Labels = append(out.Labels, l.Name)
	}
	return out
}

type comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      user      `json:"user"`
	IssueURL  string    `json:"issue_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (c comment) canonical(ref core.IssueRef) core.Comment {
	return core.Comment{ID: strconv.FormatInt(c.ID, 10), Issue: ref, Author: actorOf(c.User), Body: c.Body, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

// GetIssue implements core.IssueTracker.
func (t *Tracker) GetIssue(ctx context.Context, ref core.IssueRef) (core.Issue, error) {
	var i issue
	if err := t.c.repoCall(ctx, ref.Container, http.MethodGet, "/repos/"+ref.Container+"/issues/"+ref.ExternalID, nil, &i); err != nil {
		return core.Issue{}, err
	}
	return i.canonical(ref.Container), nil
}

// ListIssues implements core.IssueTracker. The issues endpoint also returns pull
// requests; those are filtered out.
func (t *Tracker) ListIssues(ctx context.Context, q core.IssueQuery) ([]core.Issue, error) {
	v := url.Values{"per_page": {"100"}, "sort": {"updated"}, "direction": {"asc"}, "state": {"all"}}
	if q.OpenOnly {
		v.Set("state", "open")
	}
	if !q.UpdatedSince.IsZero() {
		v.Set("since", q.UpdatedSince.UTC().Format(time.RFC3339))
	}
	out := []core.Issue{}
	err := t.c.list(ctx, q.Container, "/repos/"+q.Container+"/issues?"+v.Encode(), func(raw json.RawMessage) error {
		var i issue
		if err := json.Unmarshal(raw, &i); err != nil {
			return core.Wrap(core.KindTransient, "github.list_issues", err)
		}
		if i.PullRequest == nil && (q.Limit == 0 || len(out) < q.Limit) {
			out = append(out, i.canonical(q.Container))
		}
		return nil
	})
	return out, err
}

// ListComments implements core.IssueTracker.
func (t *Tracker) ListComments(ctx context.Context, ref core.IssueRef, since time.Time) ([]core.Comment, error) {
	v := url.Values{"per_page": {"100"}}
	if !since.IsZero() {
		v.Set("since", since.UTC().Format(time.RFC3339))
	}
	own, err := t.c.AppLogin(ctx)
	if err != nil {
		return nil, err
	}
	out := []core.Comment{}
	err = t.c.list(ctx, ref.Container, "/repos/"+ref.Container+"/issues/"+ref.ExternalID+"/comments?"+v.Encode(), func(raw json.RawMessage) error {
		var c comment
		if err := json.Unmarshal(raw, &c); err != nil {
			return core.Wrap(core.KindTransient, "github.list_comments", err)
		}
		cc := c.canonical(ref)
		cc.Own = cc.Author.Type == core.ActorBot && cc.Author.Login == own
		out = append(out, cc)
		return nil
	})
	return out, err
}

// idemMarker embeds the idempotency key in an invisible comment, because GitHub
// has no idempotency mechanism. The key is hashed so it cannot break out of the
// HTML comment.
func idemMarker(key string) string {
	return "<!-- goblins:idem:" + core.Digest(key)[:32] + " -->"
}

// PostComment implements core.IssueTracker. Before posting, it looks for an earlier
// comment by automation carrying the same marker, which is what makes a retry after
// a lost response safe. A person pasting the marker cannot suppress a comment.
func (t *Tracker) PostComment(ctx context.Context, ref core.IssueRef, d core.CommentDraft) (core.Comment, error) {
	body := d.Body
	if d.IdempotencyKey != "" {
		marker := idemMarker(d.IdempotencyKey)
		existing, err := t.ListComments(ctx, ref, time.Time{})
		if err != nil {
			return core.Comment{}, err
		}
		for _, c := range existing {
			if c.Own && strings.Contains(c.Body, marker) {
				return c, nil
			}
		}
		body += "\n\n" + marker
	}
	var c comment
	if err := t.c.repoCall(ctx, ref.Container, http.MethodPost, "/repos/"+ref.Container+"/issues/"+ref.ExternalID+"/comments", map[string]string{"body": body}, &c); err != nil {
		return core.Comment{}, err
	}
	return c.canonical(ref), nil
}

// ActorPermission implements core.IssueTracker.
func (t *Tracker) ActorPermission(ctx context.Context, ref core.IssueRef, a core.Actor) (core.Permission, error) {
	var res struct {
		Permission string `json:"permission"`
		RoleName   string `json:"role_name"`
	}
	path := "/repos/" + ref.Container + "/collaborators/" + url.PathEscape(a.Login) + "/permission"
	err := t.c.repoCall(ctx, ref.Container, http.MethodGet, path, nil, &res)
	if core.IsKind(err, core.KindNotFound) {
		return core.PermissionNone, nil
	}
	if err != nil {
		return core.PermissionNone, err
	}
	// `permission` collapses maintain and triage into write and read; the role is finer.
	switch res.RoleName {
	case "admin":
		return core.PermissionAdmin, nil
	case "maintain", "write":
		return core.PermissionWrite, nil
	case "triage":
		return core.PermissionTriage, nil
	case "read":
		return core.PermissionRead, nil
	}
	return core.ParsePermission(res.Permission), nil
}

// SetLabels implements core.IssueTracker.
func (t *Tracker) SetLabels(ctx context.Context, ref core.IssueRef, add, remove []string) error {
	base := "/repos/" + ref.Container + "/issues/" + ref.ExternalID + "/labels"
	if len(add) > 0 {
		if err := t.c.repoCall(ctx, ref.Container, http.MethodPost, base, map[string][]string{"labels": add}, nil); err != nil {
			return err
		}
	}
	for _, l := range remove {
		err := t.c.repoCall(ctx, ref.Container, http.MethodDelete, base+"/"+url.PathEscape(l), nil, nil)
		if err != nil && !core.IsKind(err, core.KindNotFound) {
			return err
		}
	}
	return nil
}

// Transition implements core.IssueTracker. GitHub issues have two statuses.
func (t *Tracker) Transition(ctx context.Context, ref core.IssueRef, status string) error {
	if status != "open" && status != "closed" {
		return core.Errorf(core.KindInvalid, "github.transition", "unknown status %q", status)
	}
	return t.c.repoCall(ctx, ref.Container, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/%s", ref.Container, ref.ExternalID), map[string]string{"state": status}, nil)
}
