package workflow

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// maxFindings bounds what a task stores of a pull request's review remarks.
const maxFindings = 200

// afterReport is where a task stops being driven by Goblins and starts following what happens
// to its pull request, until someone merges or closes it.
func (m *machine) afterReport(reason string) {
	if m.p.StopAtReport || m.t.PullRequest == nil {
		m.finish(reason)
		return
	}
	m.moveTo(core.StateAwaitingReview, reason)
	m.confirmFixes()
	m.enqueueReconcile()
}

func (m *machine) enqueueReconcile() {
	m.enqueueMain(JobReconcile, struct{}{}, "", 5)
	m.reason = "reconciling the pull request"
}

// onPullRequestEvent treats any signal about a pull request as a reason to look: what counts
// is the state an SCM reports now, not what a delivery, which could be old or replayed, said.
// Looking runs no model, and identical deliveries never get this far.
func (m *machine) onPullRequestEvent() {
	ev := m.ev
	if !m.acceptsReviewInput() {
		m.ignore("review input from someone without sufficient access")
		return
	}
	if ev.Type == core.EventPRClosed && m.t.State != core.StateAwaitingReview {
		reason := "the pull request was closed"
		if ev.PullRequest != nil && ev.PullRequest.Merged {
			reason = "the pull request was merged"
		}
		m.cancelTask(reason)
		return
	}
	if m.t.State != core.StateAwaitingReview {
		m.ignore("the pull request is looked at again once this round ends")
		return
	}
	if m.t.Kind == core.TaskReviewOnly {
		m.onReviewOnlyEvent()
		return
	}
	if m.t.CurrentJob != nil {
		m.ignore("a look at the pull request is already under way")
		m.t.PendingInput = true
		m.changed = true
		return
	}
	m.enqueueReconcile()
}

// acceptsReviewInput filters who can cause work: people with access, and listed reviewers.
// Facts about the pull request itself come from the authenticated source, not from a person.
func (m *machine) acceptsReviewInput() bool {
	switch m.ev.Type {
	case core.EventPRReviewed, core.EventReviewComment, core.EventCommentCreated:
	default:
		return true
	}
	a := m.ev.Actor
	if m.ev.Comment != nil {
		a = m.ev.Comment.Author
	}
	if isHuman(a, m.p) {
		return m.f.ActorPermission >= m.p.MinInputPermission
	}
	return slices.Contains(m.p.TrustedReviewBots, a.Login)
}

func (m *machine) onReviewOnlyEvent() {
	pr := m.ev.PullRequest
	switch {
	case m.ev.Type == core.EventPRClosed:
		m.t.CurrentJob = nil
		m.moveTo(core.StateCompleted, "the pull request was closed")
	case m.ev.Type == core.EventPRSynchronized && pr != nil && pr.HeadSHA != m.t.HeadSHA:
		m.t.HeadSHA = pr.HeadSHA
		m.t.PullRequest = pr
		m.enqueueMain(JobReview, ReviewPayload{SHA: pr.HeadSHA}, m.reviewLimitKey(), 3)
		m.moveTo(core.StateReviewing, "the pull request has a new head")
	default:
		m.ignore("nothing to do for a review-only task")
	}
}

// onHeadConflict leaves a task whose branch moved under it for a person. Resuming looks at the
// pull request again, instead of retrying a push that cannot succeed.
func (m *machine) onHeadConflict(msg string) {
	if m.t.PullRequest == nil {
		m.fail(msg)
		return
	}
	m.t.CurrentJob = nil
	m.block(msg)
	m.t.ResumeState = core.StateAwaitingReview
}

func (m *machine) onReconcileDone(o *core.JobOutcome) {
	res, ok := decode[ReconcileResult](o)
	if !ok || res.PullRequest.Number == "" {
		m.block("reading the pull request returned nothing usable")
		return
	}
	m.t.CurrentJob = nil
	pending := m.t.PendingInput
	m.t.PendingInput = false
	pr := res.PullRequest
	m.t.PullRequest = &pr
	if !pr.Open {
		m.onClosed(pr)
		return
	}
	if pr.HeadSHA != "" && pr.HeadSHA != m.t.HeadSHA {
		m.t.HeadSHA = pr.HeadSHA
	}
	m.t.Findings = mergeFindings(m.t.Findings, res.Findings)
	m.changed = true
	if m.t.ReviewedKey != reviewKey(m.t.HeadSHA) {
		m.enqueueMain(JobReview, ReviewPayload{SHA: m.t.HeadSHA}, m.reviewLimitKey(), 3)
		m.moveTo(core.StateReviewing, "the pull request has an unreviewed head")
		return
	}
	if !m.p.SkipReviewTriage && m.converse(res) {
		return
	}
	if m.feedActionable(res) {
		return
	}
	if m.applyGate(res); pending && m.t.CurrentJob == nil {
		m.enqueueReconcile() // something arrived while looking, so what was seen may be old
	}
}

func (m *machine) onClosed(pr core.PullRequest) {
	m.t.PausedUntil = time.Time{}
	if pr.Merged {
		m.finish("the pull request was merged")
		return
	}
	m.t.Reason = "the pull request was closed without merging"
	m.moveTo(core.StateCancelled, m.t.Reason)
	m.jobs = append(m.jobs, core.JobDraft{Kind: JobCleanup, IdempotencyKey: m.nextKey(JobCleanup),
		MaxAttempts: 5})
}

// mergeFindings folds what the SCM reports into what is known, keeping what has happened to
// each. A remark seen before stays the same finding; the SCM has the last word on resolution.
func mergeFindings(known, seen []core.ReviewFinding) []core.ReviewFinding {
	out := slices.Clone(known)
	for _, s := range seen {
		i := slices.IndexFunc(out, func(k core.ReviewFinding) bool { return k.ID == s.ID })
		if i < 0 {
			if s.Status == "" {
				s.Status = core.FindingOpen
			}
			out = append(out, s)
			continue
		}
		spoke := s.LastCommentID != "" && s.LastCommentID != out[i].LastCommentID
		switch {
		case s.Status == core.FindingResolved || s.Status == core.FindingStale:
			out[i].Status = s.Status
		case out[i].Status == core.FindingResolved:
			out[i].Status = core.FindingOpen // reopened
		case spoke && reopensOnReply(out[i].Status, s.Discussion):
			out[i].Status = core.FindingOpen // the reviewer answered
		}
		out[i].Body, out[i].Path, out[i].Line = s.Body, s.Path, s.Line
		out[i].Discussion, out[i].LastCommentID = s.Discussion, s.LastCommentID
	}
	if extra := len(out) - maxFindings; extra > 0 {
		out = slices.Delete(out, 0, extra)
	}

	return out
}

// feedActionable sends what people asked for, and CI failures of this commit, to the
// implementer. A failure is sent once per commit, and a finding once: a further remark is a
// new finding. Whether something got fixed is for reviewers and CI to say on the next commit.
func (m *machine) feedActionable(res ReconcileResult) bool {
	var findings []core.ReviewFinding
	for i := range m.t.Findings {
		f := &m.t.Findings[i]
		if f.Status == core.FindingOpen && f.Severity.Rank() >= m.p.ReviewMinSeverity.Rank() {
			findings = append(findings, *f)
		}
	}
	var failed []core.StatusCheck
	if m.t.ChecksHandledSHA != m.t.HeadSHA {
		for _, c := range res.Checks {
			if c.State == core.CheckFailed {
				failed = append(failed, c)
			}
		}
	}
	if len(findings) == 0 && len(failed) == 0 {
		return false
	}
	feedback := RenderReviewFeedback(findings, m.t.HeadSHA)
	if len(failed) > 0 {
		feedback = RenderFailedCI(failed, m.t.HeadSHA) + "\n\n" + feedback
		m.t.ChecksHandledSHA = m.t.HeadSHA
	}
	m.sendBack("the pull request", feedback, findings, m.t.HeadSHA)
	if m.t.State == core.StateImplementing {
		for i := range m.t.Findings {
			if f := &m.t.Findings[i]; f.Status == core.FindingOpen && f.Severity.Rank() >= m.p.ReviewMinSeverity.Rank() {
				f.Status = core.FindingAddressed
			}
		}
	}
	return true
}

func (m *machine) applyGate(res ReconcileResult) {
	gate := EvaluateGate(GateInput{Task: m.t, Result: res, Policy: m.p.Merge,
		MinPerm: m.p.MinApprovePermission})
	m.t.Gate = &gate
	m.reason = "merge policy evaluated: " + string(gate.State)
	digest := core.Digest(string(gate.State), gate.SHA, strings.Join(gate.Reasons, "\n"))
	if digest != m.t.GateDigest {
		m.t.GateDigest = digest
		m.comment(RenderGate(gate))
	}
	if gate.State == core.GateAutoMerge {
		m.enqueueMain(JobMerge, MergePayload{SHA: gate.SHA, Rule: gate.Rule}, "", 3)
		m.reason = "merging under rule " + gate.Rule
	}
}

// RenderGate tells people where a pull request stands, and is clear that nothing an automated
// reviewer said is part of why.
func RenderGate(g core.MergeGate) string {
	var b strings.Builder
	switch g.State {
	case core.GateAutoMerge:
		fmt.Fprintf(&b,
			"`%s` meets everything required, and rule `%s` allows merging it. Goblins is merging this exact commit.\n", g.SHA, g.Rule)
	case core.GateReady:
		fmt.Fprintf(&b,
			"`%s` meets the requirements that can be checked. It awaits a person to merge it.\n",
			g.SHA)
	default:
		fmt.Fprintf(&b, "`%s` is not ready to merge:\n", g.SHA)
	}
	for _, r := range g.Reasons {
		b.WriteString("\n- " + r)
	}
	b.WriteString("\n\nAn automated review is an opinion. It never counts towards this.")
	return b.String()
}
