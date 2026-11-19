package e2e

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/app"
	"github.com/ungarscool1/goblins/internal/config"
	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/providers/jira"
	"github.com/ungarscool1/goblins/internal/providers/jira/fakejira"
)

// A work profile: tickets in Jira, read and answered under a person's own account, code on
// the forge, and Claude as the only agent the tickets may reach.
func TestJiraTicketToDraftPullRequest(t *testing.T) {
	e, site, issue, step := jiraSetup(t, true)
	step(core.StateWaitingForUser)
	comments := site.Issue(issue.Key).Comments
	if len(comments) != 1 || !strings.HasPrefix(comments[0].Body, jira.Prefix) ||
		!strings.Contains(comments[0].Body, "Which database should it use?") {
		t.Fatalf("question on Jira: %+v", comments)
	}

	site.AddComment(issue.Key, lead, "Use Postgres please")
	got := step(core.StateDesignProposed)
	if !strings.Contains(lastComment(site, issue.Key), "{{/approve-design 1}}") {
		t.Fatalf("design comment:\n%s", lastComment(site, issue.Key))
	}

	site.AddComment(issue.Key, lead, fmt.Sprintf("/approve-design %d", got.DesignRevision))
	done := step(core.StateCompleted) // after a first round that fails its checks
	if done.Approval == nil ||
		done.Approval.Approver.ID != "lead-account" || done.PullRequest == nil {
		t.Fatalf("task = %s %q, approval %+v, pr %+v", done.State, done.Reason, done.Approval,
			done.PullRequest)
	}
	if len(e.gh.PullRequests(repo)) != 1 {
		t.Fatal("no draft pull request on the forge")
	}
	if !strings.Contains(lastComment(site, issue.Key), "Status report") {
		t.Fatalf("no report on Jira:\n%s", lastComment(site, issue.Key))
	}
}

var lead = fakejira.User{AccountID: "lead-account", AccountType: "atlassian"}

// A read-only Jira account is never written to: the questions wait for the operator instead.
func TestReadOnlyJiraWritesNothing(t *testing.T) {
	_, site, issue, step := jiraSetup(t, false)
	got := step(core.StateBlocked)
	if n := len(site.Issue(issue.Key).Comments); n != 0 {
		t.Fatalf("%d comments written to a read-only Jira", n)
	}
	if !strings.Contains(got.Reason, "comment") {
		t.Fatalf("reason = %q", got.Reason)
	}
	for _, r := range site.Requests {
		if !strings.HasPrefix(r, "GET ") {
			t.Fatalf("a read-only profile sent %s", r)
		}
	}
}

type jiraStep func(want core.State) core.Task

func jiraSetup(t *testing.T, write bool) (*env, *fakejira.Server, *fakejira.Issue, jiraStep) {
	t.Helper()
	e := setup(t)
	me := fakejira.User{AccountID: "me-account", AccountType: "atlassian"}
	site := fakejira.New(true, "Bearer jira-token", me, time.Now)
	t.Cleanup(site.Close)
	site.Grant("me-account", "ADD_COMMENTS", "BROWSE_PROJECTS")
	site.Grant("lead-account", "EDIT_ISSUES", "BROWSE_PROJECTS")
	t.Setenv("E2E_JIRA_TOKEN", "jira-token")

	cfg := string(e.cfgYAML)
	cfg = strings.Replace(cfg, "trackers:\n",
		fmt.Sprintf("trackers:\n  jira: {type: jira, options: "+
			"{url: %s, auth: bearer, token_env: E2E_JIRA_TOKEN, projects: [OPS], write: %t, "+
			"agents: [claude]}}\n", site.URL, write), 1)
	cfg = strings.Replace(cfg, "event_sources:\n",
		fmt.Sprintf("event_sources:\n  jira: {type: jira, "+
			"options: {url: %s, auth: bearer, token_env: E2E_JIRA_TOKEN, projects: [OPS]}}\n",
			site.URL), 1)
	cfg = strings.Replace(cfg, "    tracker: forge\n",
		"    tracker: jira\n    tracker_container: OPS\n"+
			"    poll_interval: 1ms\n", 1)
	cfg = strings.Replace(cfg, "    event_sources: [forge]\n", "    event_sources: [jira, forge]\n",
		1)
	e.cfgYAML = []byte(cfg)
	e.boot()

	issue := site.AddIssue("OPS", "Add the feature", "Please add the feature.", lead)
	ref := core.Subject{Kind: core.SubjectIssue, Provider: "jira", Container: "OPS",
		ExternalID: issue.Key}
	task := func() core.Task {
		t.Helper()
		got, err := e.store.FindTask(ctx, "app", ref)
		if err != nil {
			t.Fatalf("no task: %v\nrequests: %v", err, site.Requests)
		}
		return got
	}
	step := func(want core.State) core.Task {
		t.Helper()
		time.Sleep(5 * time.Millisecond) // Jira's clock has millisecond steps
		e.settle()
		got := task()
		if got.State != want {
			t.Fatalf("state = %s (%q), want %s\n%s", got.State, got.Reason, want, e.invocations())
		}
		return got
	}
	return e, site, issue, step
}

func lastComment(s *fakejira.Server, key string) string {
	cs := s.Issue(key).Comments
	if len(cs) == 0 {
		return ""
	}
	return cs[len(cs)-1].Body
}

// A tracker that keeps its tickets from some agents refuses a project using another one,
// before anything runs.
func TestJiraTicketsCannotReachAnotherAgent(t *testing.T) {
	e := setup(t)
	site := fakejira.New(true, "Bearer jira-token", fakejira.User{AccountID: "me"}, time.Now)
	t.Cleanup(site.Close)
	t.Setenv("E2E_JIRA_TOKEN", "jira-token")
	cfg := strings.Replace(string(e.cfgYAML), "trackers:\n",
		fmt.Sprintf("trackers:\n  jira: {type: "+
			"jira, options: {url: %s, auth: bearer, token_env: E2E_JIRA_TOKEN, projects: [OPS], "+
			"agents: [work-claude]}}\n", site.URL), 1)
	cfg = strings.Replace(cfg, "    tracker: forge\n",
		"    tracker: jira\n    tracker_container: OPS\n", 1)
	if err := e.buildOnly(cfg); err == nil || !strings.Contains(err.Error(), "only allows agents") {
		t.Fatalf("err = %v", err)
	}
}

// buildOnly wires a configuration without running anything.
func (e *env) buildOnly(cfg string) error {
	reg := e.registry()
	c, err := config.LoadBytes([]byte(cfg), os.LookupEnv, reg.Known)
	if err != nil {
		return err
	}
	_, err = app.Build(c, e.store, discard, app.Options{Registry: reg})
	return err
}
