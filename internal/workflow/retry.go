package workflow

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/ungarscool1/goblins/internal/core"
)

var retryCommand = regexp.MustCompile(`^/goblins\s+retry$`)

// decideTerminal handles an event on a finished task. The only thing that can happen to one is
// a person asking to run a failed task again: completed and cancelled tasks stay as they are.
func decideTerminal(task *core.Task, ev core.Event, f Facts, p Policy) Decision {
	c := ev.Comment
	if task.State != core.StateFailed || ev.Type != core.EventCommentCreated || c == nil ||
		!isHuman(c.Author, p) || !retryCommand.MatchString(strings.TrimSpace(firstLine(c.Body))) {
		return Decision{Ignored: "task is in a terminal state"}
	}
	m := &machine{t: cloneTask(*task), from: task.State, ev: ev, f: f, p: p}
	if f.ActorPermission < p.MinApprovePermission {
		m.reject(c, fmt.Sprintf("@%s, `/goblins retry` needs %s access.", c.Author.Login,
			p.MinApprovePermission))
		return m.decision()
	}
	m.retry(c.Author.Login)
	return m.decision()
}

// retry runs a failed task again from the step that failed, as resume does for a blocked one:
// the same job, with a fresh key, and fresh loop budgets, since a person asked. A task that
// failed before the step was recorded starts over from planning.
func (m *machine) retry(who string) {
	switch m.t.ResumeState {
	case "", core.StateBlocked, core.StateQuotaPaused, core.StateDiscovered:
		m.t.ResumeState, m.t.CurrentJob = core.StateInvestigating, nil
	}
	for _, k := range []string{"plan", "implement", "review_fix", "triage", loopLimitPause} {
		delete(m.t.Loops, k)
	}
	m.t.TokenBudgetFrom = m.t.TokensUsed
	m.comment(fmt.Sprintf("Run again by @%s, from where it failed.", who))
	m.resume("run again by " + who)
}
