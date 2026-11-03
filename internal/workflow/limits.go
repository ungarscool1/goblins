package workflow

import (
	"fmt"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// loopLimitPause counts pauses in a row, so a limit that never lifts ends with a person
// being asked rather than with retries forever.
const loopLimitPause = "limit_pause"

// maxReportedWait is the furthest a reset time reported by a CLI is believed: weekly limits
// exist, and anything past them is more likely a misread than a fact.
const maxReportedWait = 8 * 24 * time.Hour

// ReleaseHoldPayload names the limit key a person asked to try again.
type ReleaseHoldPayload struct {
	LimitKey string `json:"limit_key"`
}

// pauseForLimit waits out a usage limit, a rate limit or a lost login. The task keeps its job,
// session and branch, and comes back to exactly where it was.
func (m *machine) pauseForLimit(o *core.JobOutcome, msg string) {
	n := m.t.Loops[loopLimitPause] + 1
	if m.p.MaxLimitPauses > 0 && n > m.p.MaxLimitPauses {
		delete(m.t.Loops, loopLimitPause)
		m.block(fmt.Sprintf("the agent stayed unavailable through %d pauses: %s",
			m.p.MaxLimitPauses, msg))
		return
	}
	m.t.Loops[loopLimitPause] = n
	until := m.limitPauseEnd(o, n)
	pause := func(what string) {
		at := until.UTC().Format("Jan 2 15:04 UTC")
		m.interrupt(core.StateQuotaPaused, what+", trying again at "+at+": "+msg, until)
	}
	switch o.ErrorKind {
	case core.KindRateLimited:
		pause("agent rate limited")
	case core.KindAuthRequired:
		pause("agent login is required")
		if n == 1 {
			m.notify(core.SeverityAction, "Agent login required", msg)
			m.comment("Goblins paused: the agent needs a person to log in again on the " +
				"installation. Work resumes automatically afterwards.")
		}
	default:
		pause("agent usage limit reached")
		if n == 1 {
			m.notify(core.SeverityInfo, "Task paused: usage limit", msg)
		}
	}
}

// limitPauseEnd believes a reset time the CLI reported. Without one, each pause in a row
// waits twice as long, up to MaxLimitPause.
func (m *machine) limitPauseEnd(o *core.JobOutcome, n int) time.Time {
	if o.ErrorKind != core.KindAuthRequired && o.RetryAt.After(m.f.Now) &&
		o.RetryAt.Before(m.f.Now.Add(maxReportedWait)) {
		return o.RetryAt
	}
	base := m.p.QuotaPause
	switch o.ErrorKind {
	case core.KindRateLimited:
		base = m.p.RateLimitPause
	case core.KindAuthRequired:
		base = m.p.AuthPause
	}
	wait := base << min(n-1, 16)
	if m.p.MaxLimitPause > 0 {
		wait = min(wait, max(m.p.MaxLimitPause, base))
	}
	return m.f.Now.Add(wait)
}

// releaseHold lets a person override a limit: they may know that it lifted, or log in again.
func (m *machine) releaseHold() {
	delete(m.t.Loops, loopLimitPause)
	cur := m.t.CurrentJob
	if cur == nil || cur.LimitKey == "" {
		return
	}
	m.jobs = append(m.jobs, core.JobDraft{Kind: JobReleaseHold,
		IdempotencyKey: m.nextKey(JobReleaseHold), Priority: 10, MaxAttempts: 5,
		Payload: mustJSON(ReleaseHoldPayload{LimitKey: cur.LimitKey})})
}

// overFileBudget stops a change touching more files than the project allows, before anything
// is spent on checking or publishing it.
func (m *machine) overFileBudget(res ImplementResult) bool {
	if m.p.MaxChangedFiles <= 0 || res.ChangedFiles <= m.p.MaxChangedFiles {
		return false
	}
	m.block(fmt.Sprintf("the change touches %d files, and this project allows %d",
		res.ChangedFiles, m.p.MaxChangedFiles))
	return true
}
