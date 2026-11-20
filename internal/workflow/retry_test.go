package workflow

import (
	"testing"

	"github.com/ungarscool1/goblins/internal/core"
)

// A task whose job used up its attempts can be run again by a person, from that job.
func TestAFailedTaskRunsAgainFromItsFailedStep(t *testing.T) {
	p := DefaultPolicy()
	task := at(core.StateImplementing, JobValidate)
	failed := Decide(task, finished(task, false, core.KindInternal, "boom", nil), writeFacts(), p)
	mustState(t, failed, core.StateFailed)
	if failed.Task.ResumeState != core.StateImplementing || failed.Task.CurrentJob == nil {
		t.Fatalf("failed task = %+v", failed.Task)
	}
	// Anything else on a failed task still does nothing.
	if d := Decide(failed.Task, comment(maintainer, "/goblins resume", t0), writeFacts(),
		p); d.Task != nil {
		t.Fatalf("resume changed a failed task: %+v", d.Task)
	}
	if d := Decide(failed.Task, comment(stranger, "/goblins retry", t0),
		Facts{ActorPermission: core.PermissionRead, Now: t0}, p); d.Task != nil &&
		d.Task.State != core.StateFailed {
		t.Fatal("someone without access ran it again")
	}
	again := Decide(failed.Task, comment(maintainer, "/goblins retry", t0), writeFacts(), p)
	mustState(t, again, core.StateImplementing)
	if !hasJob(again, JobValidate) || again.Task.CurrentJob.IdempotencyKey == "cur" {
		t.Fatalf("jobs = %v, current = %+v", kinds(again), again.Task.CurrentJob)
	}
	if !CheckTransition(core.StateFailed, core.StateImplementing, core.StateImplementing) ||
		CheckTransition(core.StateFailed, core.StateAgentReady, core.StateImplementing) ||
		CheckTransition(core.StateCompleted, core.StateInvestigating, "") {
		t.Fatal("the table lets a terminal task go somewhere new")
	}
}

// A task that failed before its step was recorded plans again.
func TestAnOldFailedTaskPlansAgain(t *testing.T) {
	old := at(core.StateFailed, "")
	d := Decide(old, comment(maintainer, "/goblins retry", t0), writeFacts(), DefaultPolicy())
	mustState(t, d, core.StateInvestigating)
	if !hasJob(d, JobPlan) {
		t.Fatalf("jobs = %v", kinds(d))
	}
}
