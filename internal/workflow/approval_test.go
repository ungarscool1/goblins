package workflow

import (
	"strings"
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
)

func unattendedPolicy() Policy {
	p := DefaultPolicy()
	p.IntakeLabel = "goblins"
	p.Approval = ApprovalProfile{Unattended: true, Label: "goblins:unattended",
		AllowedPaths: []string{"docs/**", "src/**"}, MaxChangedFiles: 5,
		DeniedPaths: []string{"ci/**", "**/auth/**"}}
	return p
}

func labeled(label string, a core.Actor) core.Event {
	return core.Event{Project: "p", Type: core.EventIssueLabeled, Subject: subj, Actor: a,
		Label: label, Issue: &core.Issue{Title: "x", Body: "y", Open: true}}
}

// unattendedTask takes an item labeled by a maintainer through planning.
func unattendedTask(t *testing.T, p Policy) Decision {
	t.Helper()
	d := Decide(nil, labeled("goblins:unattended", maintainer), writeFacts(), p)
	mustState(t, d, core.StateInvestigating)
	if d.Task.Unattended == nil || d.Task.Unattended.Actor.Login != "maintainer" {
		t.Fatalf("unattended = %+v", d.Task.Unattended)
	}
	d = Decide(d.Task, finished(d.Task, true, "", "", PlanResult{Design: "the design"}),
		writeFacts(), p)
	// Nothing runs before the design is on the ticket.
	mustState(t, d, core.StateDesignProposed)
	if d.Task.Approval != nil || hasJob(d, JobImplement) {
		t.Fatalf("approved before the design was posted: %+v", d.Task.Approval)
	}
	return Decide(d.Task, posted(d.Task), writeFacts(), p)
}

// posted reports the design comment as posted.
func posted(t *core.Task) core.Event {
	return core.Event{Project: "p", Type: core.EventJobFinished, Subject: subj,
		Job: &core.JobOutcome{Kind: JobComment, Key: t.DesignCommentKey, OK: true,
			Result: mustJSON(CommentResult{PostedAt: t0})}}
}

func TestUnattendedDesignsNeedNoPerson(t *testing.T) {
	p := unattendedPolicy()
	d := unattendedTask(t, p)
	mustState(t, d, core.StateAgentReady)
	a := d.Task.Approval
	if a == nil || a.Approver.Login != "maintainer" || !strings.HasPrefix(a.Policy,
		"unattended@") ||
		a.Hash != d.Task.DesignHash || !hasJob(d, JobImplement) {
		t.Fatalf("approval = %+v", a)
	}
	// Inside the scope, the change goes on to its checks with nobody asked.
	in := Decide(d.Task, finished(d.Task, true, "", "", ImplementResult{HeadSHA: "h1",
		Changed: true, ChangedFiles: 2, ChangedPaths: []string{"docs/a.md", "src/x/y.go"}}),
		writeFacts(), p)
	mustState(t, in, core.StateImplementing)
	if !hasJob(in, JobValidate) {
		t.Fatalf("jobs = %v", kinds(in))
	}
}

// A change touching anything sensitive or outside the scope stops for a person, whatever the
// ticket or the model said about it, and a person's approval then checks that same change.
func TestAChangeOutsideTheScopeStopsForAPerson(t *testing.T) {
	p := unattendedPolicy()
	for name, res := range map[string]ImplementResult{
		"auth code":     {ChangedFiles: 1, ChangedPaths: []string{"src/auth/login.go"}},
		"denied path":   {ChangedFiles: 1, ChangedPaths: []string{"ci/pipeline.yml"}},
		"outside scope": {ChangedFiles: 1, ChangedPaths: []string{"cmd/main.go"}},
		"too many": {ChangedFiles: 6, ChangedPaths: []string{"docs/a", "docs/b", "docs/c",
			"docs/d", "docs/e", "docs/f"}},
		"unlisted": {ChangedFiles: 3, ChangedPaths: []string{"docs/a"}},
	} {
		t.Run(name, func(t *testing.T) {
			d := unattendedTask(t, p)
			res.HeadSHA, res.Changed = "h1", true
			out := Decide(d.Task, finished(d.Task, true, "", "", res), writeFacts(), p)
			mustState(t, out, core.StateDesignProposed)
			if out.Task.Approval != nil || out.Task.Unattended != nil || hasJob(out, JobValidate) ||
				out.Task.Escalated != "h1" {
				t.Fatalf("task = %+v", out.Task)
			}
			// The status comment, already on the ticket, says it waits for a person now.
			if !strings.Contains(out.Task.Status.Note, "waits for a person") {
				t.Fatalf("status note = %q", out.Task.Status.Note)
			}
			late := t0.Add(1)
			ok := Decide(out.Task, comment(maintainer, "/approve-design 1", late), writeFacts(), p)
			mustState(t, ok, core.StateAgentReady)
			if !hasJob(ok, JobValidate) || hasJob(ok, JobImplement) ||
				ok.Task.Approval.Policy != "" {
				t.Fatalf("after approval: %v, %+v", kinds(ok), ok.Task.Approval)
			}
			// The check of that change starts work, as the scheduler announces it, and from
			// there the task goes on as any other. Found on the test host, where it stayed
			// agent-ready and its pull request could not be recorded.
			cur := ok.Task.CurrentJob
			if !EmitsStart(cur.Kind) {
				t.Fatalf("a %s job is not announced", cur.Kind)
			}
			run := Decide(ok.Task, core.Event{Project: "p", Type: core.EventJobStarted,
				Subject: subj, Job: &core.JobOutcome{Kind: cur.Kind, Key: cur.IdempotencyKey}},
				writeFacts(), p)
			mustState(t, run, core.StateImplementing)
		})
	}
}

// Only a person allowed to approve can make an item run unattended, and only with the label.
func TestTheUnattendedLabelNeedsApprovalPermission(t *testing.T) {
	p := unattendedPolicy()
	triage := Facts{ActorPermission: core.PermissionTriage, Now: t0}
	if d := Decide(nil, labeled("goblins:unattended", stranger), triage, p); d.Task != nil {
		t.Fatalf("triage access started an unattended task: %+v", d.Task)
	}
	d := Decide(nil, labeled("goblins", stranger), Facts{ActorPermission: core.PermissionWrite,
		Now: t0}, p)
	if d.Task == nil || d.Task.Unattended != nil {
		t.Fatalf("the intake label made the task unattended: %+v", d.Task)
	}
	if d := Decide(nil, labeled("goblins:unattended", botUser), writeFacts(), p); d.Task != nil {
		t.Fatal("automation started an unattended task")
	}
	strict := DefaultPolicy()
	strict.IntakeLabel = "goblins"
	if d := Decide(nil, labeled("goblins:unattended", maintainer), writeFacts(),
		strict); d.Task != nil {
		t.Fatal("the label worked on a strict project")
	}
}

// A label applied while the planner asks questions is kept for when the design comes.
func TestALabelBeforeTheDesignIsKept(t *testing.T) {
	p := unattendedPolicy()
	d := Decide(at(core.StateWaitingForUser, ""), labeled("goblins:unattended", maintainer),
		writeFacts(), p)
	mustState(t, d, core.StateWaitingForUser)
	if d.Task.Unattended == nil {
		t.Fatal("the label was lost")
	}
}

// Labeling a design that waits for approval approves it by the profile.
func TestLabelingAWaitingDesignApprovesIt(t *testing.T) {
	p := unattendedPolicy()
	waiting := at(core.StateDesignProposed, "") // its comment was posted long ago
	d := Decide(waiting, labeled("goblins:unattended", maintainer), writeFacts(), p)
	mustState(t, d, core.StateAgentReady)
	if d.Task.Approval == nil || d.Task.Approval.Revision != 2 {
		t.Fatalf("approval = %+v", d.Task.Approval)
	}
}
