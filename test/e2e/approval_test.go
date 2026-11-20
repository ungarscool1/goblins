package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
)

func unattendedSetup(t *testing.T, allowed string) *env {
	t.Helper()
	e := setup(t)
	e.cfgYAML = []byte(strings.Replace(string(e.cfgYAML),
		"policy: {allow_skipped_review: true, stop_at_report: true}",
		"policy: {allow_skipped_review: true, stop_at_report: true, "+
			"approval: {profile: unattended, "+
			"label: \"goblins:unattended\", allowed_paths: ["+allowed+"]}}", 1))
	e.boot()
	e.openIssue()
	e.settle()
	i := e.gh.GetIssue(repo, e.issueNum)
	if code := e.deliver("issues", map[string]any{"action": "labeled", "issue": i,
		"label":  map[string]any{"name": "goblins:unattended"},
		"sender": maintainer}); code != http.StatusAccepted {
		t.Fatalf("webhook status %d", code)
	}
	e.settle()
	e.comment(maintainer, "Use Postgres please")
	e.settle()
	e.boot()
	e.settle()
	return e
}

// A low-risk item, allowed by a maintainer's label, goes from its ticket to a draft pull
// request through checks and a repair round, and nobody is asked to approve anything.
func TestUnattendedItemReachesADraftWithNoApproval(t *testing.T) {
	e := unattendedSetup(t, `"feature.txt", "docs/**"`)
	task := e.wantState(core.StateCompleted)
	a := task.Approval
	if a == nil || !strings.HasPrefix(a.Policy, "unattended@") || a.Approver.Login != maintainerID {
		t.Fatalf("approval = %+v", a)
	}
	if got := e.agentRuns(implementerTools); got != 2 {
		t.Fatalf("implementer rounds = %d", got)
	}
	if len(e.gh.PullRequests(repo)) != 1 {
		t.Fatal("no draft pull request")
	}
	for _, c := range e.gh.Comments(repo, e.issueNum) {
		if c.User.Type != "Bot" && strings.Contains(c.Body, "/approve-design") {
			t.Fatalf("a person was asked to approve: %q", c.Body)
		}
	}
}

// The same item, when its change touches a file outside the scope, stops before anything is
// published, and a person's approval lets that exact change go on.
func TestAChangeOutsideTheUnattendedScopeWaitsForAPerson(t *testing.T) {
	e := unattendedSetup(t, `"docs/**"`)
	task := e.wantState(core.StateDesignProposed)
	if task.Escalated == "" || len(e.gh.PullRequests(repo)) != 0 {
		t.Fatalf("task = %+v, pull requests = %d", task, len(e.gh.PullRequests(repo)))
	}
	if !strings.Contains(e.botComments(),
		"left this project's unattended scope: it changes `feature.txt`") {
		t.Fatalf("comments:\n%s", e.botComments())
	}
	rounds := e.agentRuns(implementerTools)
	e.comment(maintainer, fmt.Sprintf("/approve-design %d", task.DesignRevision))
	e.settle()
	e.boot()
	e.settle()
	task = e.wantState(core.StateCompleted)
	if task.Approval == nil || task.Approval.Policy != "" || len(e.gh.PullRequests(repo)) != 1 {
		t.Fatalf("approval = %+v", task.Approval)
	}
	if e.agentRuns(implementerTools) <= rounds-1 {
		t.Fatal("rounds went backwards")
	}
}
