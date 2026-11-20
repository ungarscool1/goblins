package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// short is how a commit is named to people: enough of it to type back.
func short(sha string) string { return sha[:min(len(sha), 12)] }

// awaitingDeploy says whether the task stopped because deploying a commit needs a person.
func (m *machine) awaitingDeploy() bool {
	return m.t.State == core.StateBlocked && m.t.Deploy != nil && m.t.Deploy.Approver == nil
}

// awaitDeploy stops a task whose commit passed its checks, when the project wants a person
// to approve each deployment to its environment. A commit already approved goes on: a later
// validation of the same commit, such as the one on the pull request, is not asked again.
func (m *machine) awaitDeploy(res ValidateResult) {
	var p ValidatePayload
	_ = json.Unmarshal(m.t.CurrentJob.Payload, &p)
	if d := m.t.Deploy; d != nil && d.SHA == p.SHA && d.Approver != nil {
		p.DeployApproved = true
		m.enqueueMain(JobValidate, p, "", 3)
		m.reason = "deployment of " + short(p.SHA) + " already approved"
		return
	}
	env := res.Environment
	reason := fmt.Sprintf("deploying %s to the %s environment waits for a person's approval",
		short(p.SHA), env)
	m.interrupt(core.StateBlocked, reason, time.Time{})
	m.progress("the checks passed on `" + short(p.SHA) + "`.")
	key := m.comment(fmt.Sprintf("%sThe checks passed on `%s`. This project asks a person before each "+
		"deployment to its `%s` environment.\n\nA maintainer can approve it with:\n\n"+
		"```\n/goblins deploy %s\n```", m.pullRequestLine(), short(p.SHA), env, short(p.SHA)))
	m.t.Deploy = &core.DeployGate{SHA: p.SHA, CommentKey: key}
	m.notify(core.SeverityAction, "Deployment awaits approval", reason)
}

// deployAsked records when the request to deploy reached the ticket: an approval must be
// written after it.
func (m *machine) deployAsked(o *core.JobOutcome) bool {
	d := m.t.Deploy
	if !o.OK || o.Kind != JobComment || d == nil || d.CommentKey == "" || o.Key != d.CommentKey {
		return false
	}
	res, ok := decode[CommentResult](o)
	if !ok || res.PostedAt.IsZero() {
		return false
	}
	m.t.Deploy = &core.DeployGate{SHA: d.SHA, RequestedAt: res.PostedAt}
	m.changed = true
	m.reason = "the request to deploy " + short(d.SHA) + " was posted"
	return true
}

// onDeployApproval lets the waiting deployment run, for the exact commit a person named, in a
// comment written after Goblins asked. The approval is recorded with who gave it.
func (m *machine) onDeployApproval(sha string, c *core.Comment) {
	d := m.t.Deploy
	switch {
	case !m.awaitingDeploy():
		m.reject(c, "No deployment is waiting for approval right now, so this was ignored.")
		return
	case !strings.HasPrefix(d.SHA, sha):
		m.reject(c, fmt.Sprintf("Ignored: the deployment waiting for approval is `%s`, not `%s`.",
			short(d.SHA), sha))
		return
	case d.RequestedAt.IsZero() || c.CreatedAt.Before(d.RequestedAt):
		m.reject(c, "Ignored: this comment predates the request to deploy "+short(d.SHA)+".")
		return
	}
	who := c.Author
	m.t.Deploy = &core.DeployGate{SHA: d.SHA, RequestedAt: d.RequestedAt, Approver: &who,
		CommentID: c.ID, At: c.CreatedAt}
	if cur := m.t.CurrentJob; cur != nil && cur.Kind == JobValidate {
		var p ValidatePayload
		_ = json.Unmarshal(cur.Payload, &p)
		p.DeployApproved = true
		next := *cur
		next.Payload = mustJSON(p)
		m.t.CurrentJob = &next
	}
	m.progress(fmt.Sprintf("deployment of `%s` approved by @%s. Deploying…", short(d.SHA),
		who.Login))
	m.resume("deployment of " + short(d.SHA) + " approved by " + who.Login)
}
