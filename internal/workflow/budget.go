package workflow

import (
	"fmt"
	"slices"

	"github.com/ungarscool1/goblins/internal/core"
)

// agentKinds are the jobs that run agents at length, which a token budget holds back.
var agentKinds = []core.JobKind{JobPlan, JobImplement, JobReview, JobTriage}

// count adds what a job's agents reported using to the task.
func (m *machine) count(o *core.JobOutcome) {
	if n := o.Usage.InputTokens + o.Usage.OutputTokens; n > 0 {
		m.t.TokensUsed += n
		m.changed = true
	}
}

// holdOverBudget keeps a task that used up its token budget from starting more agent work.
// The job it was about to start stays its current job, so a person resuming runs it, with a
// new budget. Only what agents reported counts: nothing is estimated.
func (m *machine) holdOverBudget() {
	used := m.t.TokensUsed - m.t.TokenBudgetFrom
	cur := m.t.CurrentJob
	if m.p.MaxTaskTokens <= 0 || used < m.p.MaxTaskTokens || cur == nil ||
		!slices.Contains(agentKinds, cur.Kind) {
		return
	}
	i := slices.IndexFunc(m.jobs, func(d core.JobDraft) bool {
		return d.IdempotencyKey == cur.IdempotencyKey
	})
	if i < 0 {
		return
	}
	m.jobs = slices.Delete(m.jobs, i, i+1)
	m.blockWithDetail(fmt.Sprintf("its agents used %d tokens, which reaches its budget of %d",
		used, m.p.MaxTaskTokens), "A maintainer resuming it grants another budget of the same size.")
}
