package workflow

import (
	"fmt"
	"strings"

	"github.com/ungarscool1/goblins/internal/core"
)

// sideDone routes the outcomes of side jobs that the state machine follows.
var sideDone = map[core.JobKind]func(*machine, *core.JobOutcome){
	JobReply:  (*machine).onReplyDone,
	JobStatus: (*machine).onStatusDone,
}

// progress records something people may want to know about but need not act on, such as an
// approval or a correction round. It goes into the status comment, which is edited in place
// and so notifies nobody, instead of a comment of its own.
func (m *machine) progress(note string) {
	m.t.Status.Note = note
	m.changed = true
}

// statusShown says whether a task has a status comment. Until a design is approved, the
// conversation is the status: questions and designs are comments of their own, and must stay
// so, as people answer and approve them.
func statusShown(t *core.Task) bool {
	return t.Kind == core.TaskIssue &&
		(t.Approval != nil || t.PullRequest != nil || t.Status.Note != "")
}

// syncStatus queues an update of the status comment when what it says has changed. Only one
// update is on its way at a time: its outcome runs this again, which catches up with anything
// that changed meanwhile, so updates never overtake each other.
func (m *machine) syncStatus() {
	if !m.changed || !statusShown(&m.t) || m.t.Status.Pending != "" {
		return
	}
	body := RenderStatus(&m.t, m.p)
	digest := core.Digest(body)
	if digest == m.t.Status.Posted {
		return
	}
	m.t.Status.Pending = digest
	m.jobs = append(m.jobs, core.JobDraft{Kind: JobStatus, IdempotencyKey: m.nextKey(JobStatus),
		Payload: mustJSON(StatusPayload{Body: body}), Priority: 10, MaxAttempts: 10})
}

// statusOutcome is the one event a finished task still takes: an update of its status that was
// on its way when it finished, so that the last thing it says is how it ended.
func statusOutcome(ev core.Event) bool {
	return ev.Type == core.EventJobFinished && ev.Job != nil && ev.Job.Kind == JobStatus
}

// onStatusDone records the update as done even when it failed: the next material change tries
// again, rather than every event retrying an update the tracker keeps refusing.
func (m *machine) onStatusDone(o *core.JobOutcome) {
	if res, ok := decode[StatusResult](o); ok && res.CommentID != "" {
		m.t.Status.CommentID = res.CommentID
	}
	m.t.Status.Posted, m.t.Status.Pending = m.t.Status.Pending, ""
	m.changed = true
	m.reason = "status comment updated"
	if !o.OK {
		m.reason = "status comment not updated: " + o.Message
	}
}

var stageNames = map[core.State]string{
	core.StateDiscovered: "Starting", core.StateInvestigating: "Planning",
	core.StateWaitingForUser: "Waiting for answers", core.StateDesignProposed: "Design proposed",
	core.StateAgentReady: "Implementation queued", core.StateImplementing: "Implementing",
	core.StatePROpen: "Pull request opened", core.StateReviewing: "Reviewing",
	core.StateTesting: "Testing", core.StateAwaitingReview: "Awaiting review",
	core.StateCompleted: "Done", core.StateBlocked: "Blocked", core.StateFailed: "Stopped",
	core.StateCancelled: "Cancelled", core.StateQuotaPaused: "Paused",
}

// RenderStatus says where a task stands and what, if anything, a person needs to do. It is
// built from the task alone and holds no times, so it only changes when something material
// does, and an update is only sent then.
func RenderStatus(t *core.Task, p Policy) string {
	var b strings.Builder
	b.WriteString("## Goblins status\n\n")
	fmt.Fprintf(&b, "**%s.** Next: %s.\n\n", stageNames[t.State],
		strings.TrimSuffix(nextStep(t, p), "."))
	if t.Status.Note != "" {
		fmt.Fprintf(&b, "Latest: %s\n\n", t.Status.Note)
	}
	if t.Approval != nil {
		fmt.Fprintf(&b, "- Design: revision %d, approved by @%s\n", t.Approval.Revision,
			t.Approval.Approver.Login)
	}
	if t.PullRequest != nil {
		fmt.Fprintf(&b, "- Pull request: %s (%s), at `%s`\n", prLink(*t.PullRequest),
			pullRequestState(*t.PullRequest), t.HeadSHA)
	}
	if rep := t.ValidatedReport; rep != nil && t.ValidatedSHA != "" {
		fmt.Fprintf(&b, "- Validation of `%s`: %s. %s\n", t.ValidatedSHA, rep.Status,
			strings.TrimSpace(rep.Summary))
		for _, c := range rep.Checks {
			fmt.Fprintf(&b, "  - %s\n", checkLine(c))
		}
	}
	if r := t.LastReview; r != nil {
		fmt.Fprintf(&b, "- Automated review of `%s`%s: %s\n", r.SHA, by(r.Provider),
			reviewLine(*r))
	}
	if n := t.Loops["review_fix"]; n > 0 {
		fmt.Fprintf(&b, "- Corrections after review: %d of %d\n", n, p.MaxReviewFixLoops)
	}
	if g := t.Gate; g != nil && t.State == core.StateAwaitingReview {
		b.WriteString("\n" + renderGate(*g, "`"+g.SHA+"`") + "\n")
	}
	fmt.Fprintf(&b, "\nThis comment is updated in place. Questions, designs and anything that "+
		"needs you get comments of their own. The full history is in the audit log: "+
		"`goblins audit --task %s`.", t.ID)
	return b.String()
}

func reviewLine(r core.ReviewNote) string {
	switch {
	case r.Verdict == core.VerdictSkipped:
		return "skipped, as no reviewer is configured or none was available"
	case r.Findings == 0:
		return "no findings"
	case r.Findings == 1:
		return "1 finding, in its review on the pull request"
	}
	return fmt.Sprintf("%d findings, in its review on the pull request", r.Findings)
}

func by(provider string) string {
	if provider == "" {
		return ""
	}
	return " by " + provider
}

func nextStep(t *core.Task, p Policy) string {
	switch t.State {
	case core.StateWaitingForUser:
		return "answer the questions in the latest Goblins comment"
	case core.StateDesignProposed:
		return fmt.Sprintf("read design revision %d, then approve it with `%s %d` or comment",
			t.DesignRevision, p.ApproveCommand, t.DesignRevision)
	case core.StateAwaitingReview:
		return gateStep(t.Gate)
	case core.StateBlocked:
		return "fix the cause, then comment `/goblins resume`" + because("Blocked", t.Reason)
	case core.StateQuotaPaused:
		return "nothing, Goblins resumes on its own" + because("Paused", t.Reason)
	case core.StateCompleted:
		if pr := t.PullRequest; pr != nil && !pr.Merged {
			return "review the pull request, and merge it if it is right. Goblins does not merge it"
		}
		return "nothing"
	case core.StateCancelled:
		return "nothing"
	case core.StateFailed:
		return "nothing can resume this" + because("It stopped", t.Reason)
	}
	return "nothing for now, Goblins is working"
}

func because(what, reason string) string {
	if reason == "" {
		return ""
	}
	return ". " + what + " because " + reason
}

func pullRequestState(pr core.PullRequest) string {
	switch {
	case pr.Merged:
		return "merged"
	case pr.Draft:
		return "draft, not merged"
	}
	return "not merged"
}

func gateStep(g *core.MergeGate) string {
	switch {
	case g == nil:
		return "review the pull request"
	case g.State == core.GateAutoMerge:
		return "nothing, Goblins is merging it"
	case g.State == core.GateReady:
		return "merge the pull request when you are happy with it"
	}
	return "review the pull request. What it still needs is below"
}
