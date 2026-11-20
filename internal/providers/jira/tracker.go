package jira

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// Jira project and issue keys. Only these reach JQL and URL paths, so no ticket text can
// change a query or a path.
var (
	projectKey = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	issueKey   = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}-[1-9][0-9]{0,15}$`)
)

// propertyKey marks comments this integration posted. It is stored with the comment in the
// same request, so a retry finds it and posts nothing twice.
const propertyKey = "goblins"

// Prefix opens every comment Goblins writes, since it writes under a person's account.
const Prefix = "*Goblins (automated):*\n\n"

const issueFields = "summary,description,status,labels,reporter,created,updated,project"

// Tracker implements core.IssueTracker for an allow-list of Jira projects.
type Tracker struct {
	name     string
	c        *Client
	projects []string
	write    bool
	agents   []string

	log      *slog.Logger
	warnOnce sync.Once

	mu    sync.Mutex
	perms map[string]cachedPermission
}

type cachedPermission struct {
	p  core.Permission
	at time.Time
}

// TrackerOptions configures NewTracker.
type TrackerOptions struct {
	Projects []string
	// Write lets Goblins comment, label and transition as the account. Without it, Goblins
	// only reads, and questions go to the operator's inbox instead.
	Write bool
	// Agents, when set, are the only agents projects on this tracker may use.
	Agents []string
	Log    *slog.Logger
}

// NewTracker returns a tracker.
func NewTracker(name string, c *Client, o TrackerOptions) (*Tracker, error) {
	if len(o.Projects) == 0 {
		return nil, core.Errorf(core.KindInvalid, "jira.tracker", "projects allow-list is required")
	}
	for _, p := range o.Projects {
		if !projectKey.MatchString(p) {
			return nil, core.Errorf(core.KindInvalid, "jira.tracker", "%q is not a project key", p)
		}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Tracker{name: name, c: c, projects: o.Projects, write: o.Write, agents: o.Agents,
		log: o.Log, perms: map[string]cachedPermission{}}, nil
}

// Info implements core.Provider. Collapsible sections are not offered: Jira's markup has
// an expand macro, but not one every site renders.
func (t *Tracker) Info() core.ProviderInfo {
	caps := []core.Capability{core.CapTrackerPermissions}
	if t.write {
		caps = append(caps, core.CapTrackerCommentWrite, core.CapTrackerLabels,
			core.CapTrackerTransitions)
	}
	return info(core.ProviderTracker, t.name, caps...)
}

// AllowedAgents implements core.AgentBoundary.
func (t *Tracker) AllowedAgents() []string { return t.agents }

// Serves implements core.ContainerScoped.
func (t *Tracker) Serves(container string) bool { return slices.Contains(t.projects, container) }

func (t *Tracker) check(ref core.IssueRef) error {
	if !slices.Contains(t.projects, ref.Container) {
		return core.Errorf(core.KindForbidden, "jira.tracker", "project is not allowed")
	}
	if !issueKey.MatchString(ref.ExternalID) ||
		!strings.HasPrefix(ref.ExternalID, ref.Container+"-") {
		return core.Errorf(core.KindNotFound, "jira.tracker", "no such issue in this project")
	}
	return nil
}

func (t *Tracker) writable(capability core.Capability) error {
	if !t.write {
		return core.Unsupported(t.Info(), capability)
	}
	return nil
}

// GetIssue implements core.IssueTracker.
func (t *Tracker) GetIssue(ctx context.Context, ref core.IssueRef) (core.Issue, error) {
	if err := t.check(ref); err != nil {
		return core.Issue{}, err
	}
	var i issue
	err := t.c.get(ctx, "/rest/api/2/issue/"+ref.ExternalID, url.Values{"fields": {issueFields}},
		&i)
	if err != nil {
		return core.Issue{}, err
	}
	// A key can move to another project. What it is now decides, not what was asked.
	if i.Fields.Project.Key != ref.Container {
		return core.Issue{}, core.Errorf(core.KindNotFound, "jira.issue",
			"the issue left the project")
	}
	return i.canonical(t.c.base), nil
}

// ListIssues implements core.IssueTracker.
func (t *Tracker) ListIssues(ctx context.Context, q core.IssueQuery) ([]core.Issue, error) {
	if !slices.Contains(t.projects, q.Container) {
		return nil, core.Errorf(core.KindForbidden, "jira.tracker", "project is not allowed")
	}
	jql := fmt.Sprintf(`project = "%s"`, q.Container)
	if q.OpenOnly {
		jql += " AND statusCategory != Done"
	}
	if !q.UpdatedSince.IsZero() {
		jql += " AND " + updatedSince(t.c.now(), q.UpdatedSince)
	}
	jql += " ORDER BY updated ASC"
	var out []core.Issue
	_, err := t.c.search(ctx, jql, "", q.Limit, func(i issue) error {
		out = append(out, i.canonical(t.c.base))
		return nil
	})
	return out, err
}

// updatedSince is a JQL condition relative to now. JQL reads absolute times in the account's
// own time zone, which Goblins does not know, so a relative one is exact where they are not.
func updatedSince(now, since time.Time) string {
	minutes := int(now.Sub(since).Minutes()) + 2
	return "updated >= -" + strconv.Itoa(max(minutes, 1)) + "m"
}

// ListComments implements core.IssueTracker.
func (t *Tracker) ListComments(ctx context.Context, ref core.IssueRef,
	since time.Time) ([]core.Comment, error) {
	if err := t.check(ref); err != nil {
		return nil, err
	}
	me, err := t.c.Myself(ctx)
	if err != nil {
		return nil, err
	}
	var out []core.Comment
	err = t.c.comments(ctx, ref.ExternalID, func(c comment) error {
		cc := c.canonical(ref, me)
		if since.IsZero() || !cc.UpdatedAt.Before(since) {
			out = append(out, cc)
		}
		return nil
	})
	return out, err
}

// PostComment implements core.IssueTracker.
func (t *Tracker) PostComment(ctx context.Context, ref core.IssueRef,
	d core.CommentDraft) (core.Comment, error) {
	if err := t.writable(core.CapTrackerCommentWrite); err != nil {
		return core.Comment{}, err
	}
	if err := t.check(ref); err != nil {
		return core.Comment{}, err
	}
	me, err := t.c.Myself(ctx)
	if err != nil {
		return core.Comment{}, err
	}
	var found *core.Comment
	err = t.c.comments(ctx, ref.ExternalID, func(c comment) error {
		if d.IdempotencyKey != "" && c.idempotencyKey() == d.IdempotencyKey &&
			c.Author.id() == me.id() {
			cc := c.canonical(ref, me)
			found = &cc
		}
		return nil
	})
	if err != nil || found != nil {
		return deref(found), err
	}
	in := map[string]any{"body": Prefix + ToWiki(d.Body)}
	if d.IdempotencyKey != "" {
		in["properties"] = []map[string]any{{"key": propertyKey,
			"value": map[string]string{"idempotency_key": d.IdempotencyKey}}}
	}
	var c comment
	if err := t.c.call(ctx, "POST", "/rest/api/2/issue/"+ref.ExternalID+"/comment", in,
		&c); err != nil {
		return core.Comment{}, err
	}
	cc := c.canonical(ref, me)
	cc.Own, cc.Author.Type = true, core.ActorBot
	return cc, nil
}

func deref(c *core.Comment) core.Comment {
	if c == nil {
		return core.Comment{}
	}
	return *c
}

// Jira permissions, from the most access to the least. Each maps to a canonical level.
var permissionLevels = []struct {
	jira  string
	level core.Permission
}{
	{"ADMINISTER_PROJECTS", core.PermissionAdmin},
	{"EDIT_ISSUES", core.PermissionWrite},
	{"TRANSITION_ISSUES", core.PermissionTriage},
	{"BROWSE_PROJECTS", core.PermissionRead},
}

const permissionCacheFor = time.Minute

// ActorPermission implements core.IssueTracker. It asks Jira which of the levels the actor
// has on this very issue, so issue security levels count too. The account's own permissions
// need nothing more than the account; anyone else's can only be looked up by an account that
// administers the project. Without that, others have none: their words reach nobody, and
// approvals are the account's own.
func (t *Tracker) ActorPermission(ctx context.Context, ref core.IssueRef,
	a core.Actor) (core.Permission, error) {
	if err := t.check(ref); err != nil {
		return core.PermissionNone, err
	}
	who := a.ID
	if who == "" {
		who = a.Login
	}
	if who == "" {
		return core.PermissionNone, nil
	}
	key := ref.ExternalID + "|" + who
	t.mu.Lock()
	if c, ok := t.perms[key]; ok && t.c.now().Sub(c.at) < permissionCacheFor {
		t.mu.Unlock()
		return c.p, nil
	}
	t.mu.Unlock()
	me, err := t.c.Myself(ctx)
	if err != nil {
		return core.PermissionNone, err
	}
	var p core.Permission
	if (a.ID != "" && a.ID == me.id()) || (a.ID == "" && a.Login == me.login()) {
		p, err = t.ownPermission(ctx, ref)
	} else {
		p, err = t.othersPermission(ctx, ref, a, who)
	}
	if err != nil {
		return core.PermissionNone, err
	}
	t.mu.Lock()
	t.perms[key] = cachedPermission{p: p, at: t.c.now()}
	t.mu.Unlock()
	return p, nil
}

// ownPermission reads the account's own permissions on the issue, which any account may.
func (t *Tracker) ownPermission(ctx context.Context, ref core.IssueRef) (core.Permission, error) {
	names := make([]string, len(permissionLevels))
	for i, l := range permissionLevels {
		names[i] = l.jira
	}
	var out struct {
		Permissions map[string]struct {
			Have bool `json:"havePermission"`
		} `json:"permissions"`
	}
	q := url.Values{"issueKey": {ref.ExternalID}, "permissions": {strings.Join(names, ",")}}
	if err := t.c.get(ctx, "/rest/api/2/mypermissions", q, &out); err != nil {
		return core.PermissionNone, err
	}
	for _, l := range permissionLevels {
		if out.Permissions[l.jira].Have {
			return l.level, nil
		}
	}
	return core.PermissionNone, nil
}

// othersPermission looks someone else up, which Jira allows to project administrators only.
// Refused, they have no permission, which the log says once.
func (t *Tracker) othersPermission(ctx context.Context, ref core.IssueRef, a core.Actor,
	who string) (core.Permission, error) {
	d, err := t.c.Deployment(ctx)
	if err != nil {
		return core.PermissionNone, err
	}
	param := "accountId"
	if d == DeploymentDataCenter {
		// Data Center looks people up by user name.
		param, who = "username", a.Login
	}
	for _, l := range permissionLevels {
		var users []user
		q := url.Values{"permissions": {l.jira}, "issueKey": {ref.ExternalID}, param: {who}}
		err := t.c.get(ctx, "/rest/api/2/user/permission/search", q, &users)
		if core.IsKind(err, core.KindForbidden) {
			t.warnOnce.Do(func() {
				t.log.Warn("this Jira account cannot look up other people's permissions, which "+
					"needs Administer Projects: only its own comments and approvals count",
					"tracker", t.name)
			})
			return core.PermissionNone, nil
		}
		if err != nil {
			return core.PermissionNone, err
		}
		if slices.ContainsFunc(users, func(u user) bool {
			return u.id() == a.ID || u.login() == who
		}) {
			return l.level, nil
		}
	}
	return core.PermissionNone, nil
}

// SetLabels implements core.IssueTracker.
func (t *Tracker) SetLabels(ctx context.Context, ref core.IssueRef, add, remove []string) error {
	if err := t.writable(core.CapTrackerLabels); err != nil {
		return err
	}
	if err := t.check(ref); err != nil {
		return err
	}
	var ops []map[string]string
	for _, l := range add {
		ops = append(ops, map[string]string{"add": l})
	}
	for _, l := range remove {
		ops = append(ops, map[string]string{"remove": l})
	}
	if len(ops) == 0 {
		return nil
	}
	return t.c.call(ctx, "PUT", "/rest/api/2/issue/"+ref.ExternalID,
		map[string]any{"update": map[string]any{"labels": ops}}, nil)
}

// Transition implements core.IssueTracker. The status is matched by the name of the status a
// transition leads to, or of the transition itself, as workflows differ from site to site.
func (t *Tracker) Transition(ctx context.Context, ref core.IssueRef, status string) error {
	if err := t.writable(core.CapTrackerTransitions); err != nil {
		return err
	}
	if err := t.check(ref); err != nil {
		return err
	}
	var out struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   struct {
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	path := "/rest/api/2/issue/" + ref.ExternalID + "/transitions"
	if err := t.c.get(ctx, path, nil, &out); err != nil {
		return err
	}
	for _, tr := range out.Transitions {
		if strings.EqualFold(tr.To.Name, status) || strings.EqualFold(tr.Name, status) {
			return t.c.call(ctx, "POST", path,
				map[string]any{"transition": map[string]string{"id": tr.ID}}, nil)
		}
	}
	return core.Errorf(core.KindUnsupported, "jira.transition",
		"the issue's workflow has no transition to %q from where it is", status)
}

func info(kind core.ProviderKind, name string, caps ...core.Capability) core.ProviderInfo {
	return core.ProviderInfo{Kind: kind, Type: typeName, Name: name,
		ContractVersion: core.ContractVersion, Capabilities: caps}
}
