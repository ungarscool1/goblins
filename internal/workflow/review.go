package workflow

import (
	"cmp"
	"fmt"

	"github.com/ungarscool1/goblins/internal/core"
)

func (m *machine) onPublishDone(o *core.JobOutcome) {
	res, ok := decode[PublishResult](o)
	if !ok || res.PullRequest.Number == "" {
		m.block("publishing returned no pull request")
		return
	}
	pr := res.PullRequest
	m.t.PullRequest = &pr
	if pr.HeadSHA != "" {
		m.t.HeadSHA = pr.HeadSHA
	}
	if res.Created {
		m.comment(fmt.Sprintf("Draft pull request opened: %s", prLink(pr)))
	} else {
		m.comment(fmt.Sprintf("Pull request updated: %s, now at commit `%s`.", prLink(pr),
			m.t.HeadSHA))
	}
	m.moveTo(core.StatePROpen, "pull request "+pr.Number+" open")
	if m.t.ReviewedKey == reviewKey(m.t.HeadSHA) {
		m.startFinalValidation("this commit was already reviewed")
		return
	}
	m.enqueueMain(JobReview, ReviewPayload{SHA: m.t.HeadSHA}, m.reviewLimitKey(), 3)
}

func reviewKey(sha string) string { return sha + "|" + ReviewPromptVersion }

func (m *machine) reviewLimitKey() string {
	if m.p.ReviewerLimitKey != "" {
		return m.p.ReviewerLimitKey
	}
	return m.p.ImplementerLimitKey
}

func (m *machine) onReviewDone(o *core.JobOutcome) {
	res, ok := decode[ReviewResult](o)
	if !ok || res.Result.Verdict == "" {
		m.block("the review returned no verdict")
		return
	}
	sha := cmp.Or(res.SHA, m.t.HeadSHA)
	m.t.ReviewedKey = reviewKey(sha)
	m.t.CorrectionFeedback = ""
	if m.t.Kind == core.TaskReviewOnly {
		// Publishing the review was the whole job; what to do about it is the author's call.
		m.t.CurrentJob = nil
		m.moveTo(core.StateCompleted, "review published")
		return
	}
	actionable := m.actionable(res.Result.Findings)
	switch {
	case len(actionable) > 0:
		m.correct(res.Result, actionable, sha)
	case res.Result.Verdict == core.VerdictChangesRequested:
		m.block("review requested changes: " + res.Result.Summary)
	default:
		m.startFinalValidation("review finished: " + string(res.Result.Verdict))
	}
}

// actionable picks the findings worth another implementation round.
func (m *machine) actionable(findings []core.ReviewFinding) []core.ReviewFinding {
	var out []core.ReviewFinding
	for _, f := range findings {
		if f.Severity.Rank() >= m.p.ReviewMinSeverity.Rank() && f.Confidence != core.ConfidenceLow {
			out = append(out, f)
		}
	}
	return out
}

// correct sends the implementer back with a review's findings, a bounded number of times.
// The approval still has to hold, as the findings are data and cannot widen the design.
func (m *machine) correct(res core.ReviewResult, findings []core.ReviewFinding, sha string) {
	feedback := RenderReviewFeedback(findings, sha)
	retry := ImplementPayload{Revision: m.t.DesignRevision, Hash: m.t.DesignHash,
		ReviewFeedback: feedback}
	if m.t.Approval == nil {
		m.block("the review raised findings, but this task has no approved design to correct against")
		return
	}
	if m.t.Loops["review_fix"] >= m.p.MaxReviewFixLoops {
		// Not enqueued, but recorded: if a person resumes, this is the work to do.
		m.t.CurrentJob = &core.JobDraft{Kind: JobImplement, IdempotencyKey: m.nextKey(JobImplement),
			Payload: mustJSON(retry), LimitKey: m.p.ImplementerLimitKey, MaxAttempts: 3}
		m.t.CorrectionFeedback = feedback
		m.blockWithDetail(fmt.Sprintf("the review still raises %d findings after %d correction rounds", len(findings), m.p.MaxReviewFixLoops), RenderFindingList(findings))
		return
	}
	m.t.Loops["review_fix"]++
	m.t.Loops["implement"] = 1
	m.t.CorrectionFeedback = feedback
	m.enqueueMain(JobImplement, retry, m.p.ImplementerLimitKey, 3)
	m.moveTo(core.StateImplementing, fmt.Sprintf("review raised %d findings on %s", len(findings),
		sha))
	m.comment(fmt.Sprintf("The review by %s raised %d findings on `%s`. Correction round %d of %d is queued.", cmp.Or(res.Provider, "the reviewer"), len(findings), sha, m.t.Loops["review_fix"], m.p.MaxReviewFixLoops))
}

func (m *machine) startFinalValidation(reason string) {
	reuse := m.t.ValidatedSHA != "" && m.t.ValidatedSHA == m.t.HeadSHA
	m.enqueueMain(JobValidate, ValidatePayload{Stage: StageFinal, SHA: m.t.HeadSHA, Reuse: reuse},
		"", 3)
	m.moveTo(core.StateTesting, reason)
}

func (m *machine) finish(reason string) {
	m.jobs = append(m.jobs, core.JobDraft{Kind: JobCleanup, IdempotencyKey: m.nextKey(JobCleanup),
		MaxAttempts: 5})
	m.moveTo(core.StateCompleted, reason)
}
