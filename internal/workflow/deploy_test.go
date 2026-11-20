package workflow

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

const deploySHA = "0123456789abcdef0123456789abcdef01234567"

func passed() core.ValidationReport {
	return core.ValidationReport{Status: core.ValidationPassed, Summary: "all passed"}
}

// awaitingDeployment is a task whose commit passed its checks, with its deployment waiting.
func awaitingDeployment(t *testing.T) Decision {
	t.Helper()
	task := at(core.StateImplementing, JobValidate)
	task.CurrentJob.Payload = mustJSON(ValidatePayload{Stage: StagePrePR, SHA: deploySHA})
	d := Decide(task, finished(task, true, "", "", ValidateResult{Report: passed(),
		DeployPending: true, Environment: "dev"}), writeFacts(), DefaultPolicy())
	mustState(t, d, core.StateBlocked)
	if d.Task.Deploy == nil || d.Task.Deploy.SHA != deploySHA || d.Task.Deploy.Approver != nil ||
		hasJob(d, JobValidate) || hasJob(d, JobPublish) || !hasJob(d, JobComment) {
		t.Fatalf("deploy = %+v, jobs = %v", d.Task.Deploy, kinds(d))
	}
	if !strings.Contains(d.Task.Reason, "dev environment") {
		t.Fatalf("reason = %q", d.Task.Reason)
	}
	// Nobody can approve a request that is not on the ticket yet.
	early := Decide(d.Task, comment(maintainer, "/goblins deploy "+deploySHA[:7],
		t0.Add(time.Minute)), writeFacts(), DefaultPolicy())
	if hasJob(early, JobValidate) {
		t.Fatalf("approved before it was asked: %v", kinds(early))
	}
	asked := core.Event{Project: "p", Type: core.EventJobFinished, Subject: subj,
		Job: &core.JobOutcome{Kind: JobComment, Key: d.Task.Deploy.CommentKey, OK: true,
			Result: mustJSON(CommentResult{PostedAt: t0})}}
	d = Decide(d.Task, asked, writeFacts(), DefaultPolicy())
	if d.Task == nil || !d.Task.Deploy.RequestedAt.Equal(t0) || d.Task.Deploy.CommentKey != "" {
		t.Fatalf("request not recorded as posted: %+v", d)
	}
	return d
}

func TestADeploymentWaitsForAPersonsApproval(t *testing.T) {
	d := awaitingDeployment(t)
	later := t0.Add(time.Minute)
	for name, c := range map[string]struct {
		ev core.Event
		f  Facts
	}{
		"another commit": {comment(maintainer, "/goblins deploy abcdef1", later), writeFacts()},
		"too little access": {comment(stranger, "/goblins deploy "+deploySHA[:7], later),
			Facts{ActorPermission: core.PermissionRead, Now: later}},
		"written before it was asked": {comment(maintainer, "/goblins deploy "+deploySHA[:7],
			t0.Add(-time.Minute)), writeFacts()},
		"resume instead": {comment(maintainer, "/goblins resume", later), writeFacts()},
	} {
		r := Decide(d.Task, c.ev, c.f, DefaultPolicy())
		if r.Task != nil && (r.Task.State != core.StateBlocked || hasJob(r, JobValidate)) {
			t.Errorf("%s: state %s, jobs %v", name, r.Task.State, kinds(r))
		}
	}

	ok := Decide(d.Task, comment(maintainer, "/goblins deploy "+deploySHA[:7], later), writeFacts(),
		DefaultPolicy())
	mustState(t, ok, core.StateImplementing)
	var p ValidatePayload
	if len(ok.Jobs) == 0 || json.Unmarshal(ok.Task.CurrentJob.Payload, &p) != nil ||
		!p.DeployApproved || p.SHA != deploySHA || ok.Task.CurrentJob.Kind != JobValidate {
		t.Fatalf("jobs = %v, payload = %+v", kinds(ok), p)
	}
	if a := ok.Task.Deploy; a.Approver == nil || a.Approver.Login != "maintainer" {
		t.Fatalf("deploy = %+v", a)
	}

	// The same commit validated again, on the pull request, is not asked again.
	again := Decide(ok.Task, finished(ok.Task, true, "", "", ValidateResult{Report: passed(),
		DeployPending: true, Environment: "dev"}), writeFacts(), DefaultPolicy())
	if again.Task.State == core.StateBlocked || !hasJob(again, JobValidate) {
		t.Fatalf("asked again: %s, jobs %v", again.Task.State, kinds(again))
	}
}

func TestAnOverBudgetTaskStopsBeforeMoreAgentWork(t *testing.T) {
	p := DefaultPolicy()
	p.MaxTaskTokens = 1000
	task := at(core.StateImplementing, JobValidate)
	task.Loops["implement"] = 1
	failed := core.ValidationReport{Status: core.ValidationFailed, Summary: "a test failed"}
	ev := finished(task, true, "", "", ValidateResult{Report: failed})
	ev.Job.Usage = core.AgentUsage{InputTokens: 900, OutputTokens: 200}
	d := Decide(task, ev, writeFacts(), p)
	mustState(t, d, core.StateBlocked)
	if d.Task.TokensUsed != 1100 || hasJob(d, JobImplement) ||
		d.Task.CurrentJob == nil || d.Task.CurrentJob.Kind != JobImplement {
		t.Fatalf("tokens = %d, jobs = %v, current = %+v", d.Task.TokensUsed, kinds(d),
			d.Task.CurrentJob)
	}
	r := Decide(d.Task, comment(maintainer, "/goblins resume", t0.Add(time.Minute)),
		writeFacts(), p)
	mustState(t, r, core.StateImplementing)
	if !hasJob(r, JobImplement) || r.Task.TokenBudgetFrom != 1100 {
		t.Fatalf("jobs = %v, budget from %d", kinds(r), r.Task.TokenBudgetFrom)
	}

	// Under the budget, nothing changes.
	p.MaxTaskTokens = 5000
	if d := Decide(task, ev, writeFacts(), p); !hasJob(d, JobImplement) {
		t.Fatalf("jobs = %v", kinds(d))
	}
}
