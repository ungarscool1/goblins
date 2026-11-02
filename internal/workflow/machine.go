package workflow

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// Policy holds per-project workflow rules.
type Policy struct {
	// ApproveCommand is the comment command approving a design revision.
	ApproveCommand string
	// MinApprovePermission is the least access needed to approve or to operate.
	MinApprovePermission core.Permission
	// MinInputPermission is the least access for comments to reach the planner.
	MinInputPermission core.Permission
	// IntakeLabel, when set, makes an item enter the workflow only when a permitted
	// actor applies this label. Otherwise a permitted author opening one suffices.
	IntakeLabel string
	// ReviewPullRequests starts review-only tasks for pull requests by humans.
	ReviewPullRequests bool
	// BotLogins are identities that must never count as human approval.
	BotLogins []string

	MaxPlanLoops      int
	MaxImplementLoops int
	// MaxReviewFixLoops bounds how often a review can send the implementer back.
	MaxReviewFixLoops int
	// ReviewMinSeverity is the least severity of a finding worth a correction round. Those
	// below it, and those the reviewer has low confidence in, are published suggestions.
	ReviewMinSeverity core.ReviewSeverity
	QuotaPause        time.Duration
	AuthPause         time.Duration
	// AgentLimitKeys groups agent jobs per account, for concurrency limits.
	PlannerLimitKey     string
	ImplementerLimitKey string
	ReviewerLimitKey    string
}

// DefaultPolicy returns conservative defaults.
func DefaultPolicy() Policy {
	return Policy{
		ApproveCommand:       "/approve-design",
		MinApprovePermission: core.PermissionWrite,
		MinInputPermission:   core.PermissionWrite,
		MaxPlanLoops:         8,
		MaxImplementLoops:    3,
		MaxReviewFixLoops:    3,
		ReviewMinSeverity:    core.ReviewMedium,
		QuotaPause:           30 * time.Minute,
		AuthPause:            time.Hour,
	}
}

// transitions is the complete table of allowed state changes. docs/workflow.md
// documents it; Decide's results are checked against it by CheckTransition.
var transitions = map[core.State][]core.State{
	core.StateDiscovered:     {core.StateInvestigating, core.StateReviewing},
	core.StateInvestigating:  {core.StateWaitingForUser, core.StateDesignProposed, core.StateInvestigating},
	core.StateWaitingForUser: {core.StateInvestigating},
	core.StateDesignProposed: {core.StateAgentReady, core.StateInvestigating},
	core.StateAgentReady:     {core.StateImplementing, core.StateInvestigating},
	core.StateImplementing:   {core.StatePROpen, core.StateInvestigating},
	core.StatePROpen:         {core.StateReviewing, core.StateTesting},
	core.StateReviewing:      {core.StateTesting, core.StateCompleted, core.StateImplementing},
	core.StateTesting:        {core.StateCompleted},
}

// Any active state may also become blocked, failed, cancelled or quota-paused,
// and leave a pause for the state it came from.
var interruptStates = []core.State{core.StateBlocked, core.StateFailed, core.StateCancelled, core.StateQuotaPaused}

// Terminal reports whether no further transition can happen from s.
func Terminal(s core.State) bool {
	return s == core.StateCompleted || s == core.StateFailed || s == core.StateCancelled
}

// CheckTransition reports whether the table allows from -> to, where resume is
// the task's recorded state to return to.
func CheckTransition(from, to, resume core.State) bool {
	if Terminal(from) {
		return false
	}
	if from == to || slices.Contains(transitions[from], to) {
		return true
	}
	if from == core.StateBlocked || from == core.StateQuotaPaused {
		return to == resume || slices.Contains(interruptStates, to)
	}
	return slices.Contains(interruptStates, to)
}

// Facts are looked up by the engine before deciding. Keeping them out of Decide
// keeps it pure, which is what makes the rules testable and replayable.
type Facts struct {
	ActorPermission core.Permission
	Now             time.Time
}

// Decision is the result of Decide.
type Decision struct {
	Task        *core.Task // nil when the event creates or changes nothing
	IsNew       bool
	From        core.State
	Reason      string
	Design      *core.Design
	Jobs        []core.JobDraft
	CancelKinds []core.JobKind
	Ignored     string // why the event had no effect, for logs
}

// TaskID derives a stable ID, so racing discovery of one item cannot make two tasks.
func TaskID(project string, s core.Subject) string {
	return "t-" + core.Digest(project, s.Key())[:20]
}

// Decide computes what an event means for a task. task is nil if none exists.
func Decide(task *core.Task, ev core.Event, f Facts, p Policy) Decision {
	if task == nil {
		return decideNew(ev, f, p)
	}
	if Terminal(task.State) {
		return Decision{Ignored: "task is in a terminal state"}
	}
	m := &machine{t: cloneTask(*task), from: task.State, ev: ev, f: f, p: p}
	m.step()
	return m.decision()
}

func cloneTask(t core.Task) core.Task {
	t.Loops = maps.Clone(t.Loops)
	t.Sessions = maps.Clone(t.Sessions)
	if t.Loops == nil {
		t.Loops = map[string]int{}
	}
	if t.Sessions == nil {
		t.Sessions = map[core.AgentRole]string{}
	}
	return t
}

func isHuman(a core.Actor, p Policy) bool {
	return a.Type == core.ActorHuman && !slices.Contains(p.BotLogins, a.Login)
}

func decideNew(ev core.Event, f Facts, p Policy) Decision {
	if !isHuman(ev.Actor, p) {
		return Decision{Ignored: "automation cannot start work"}
	}
	switch ev.Type {
	case core.EventIssueOpened, core.EventIssueLabeled:
		if ev.Issue == nil || !ev.Issue.Open {
			return Decision{Ignored: "no open issue in event"}
		}
		if p.IntakeLabel != "" && (ev.Type != core.EventIssueLabeled || ev.Label != p.IntakeLabel) {
			return Decision{Ignored: "intake label not applied"}
		}
		if p.IntakeLabel == "" && ev.Type != core.EventIssueOpened {
			return Decision{Ignored: "label is not an intake signal for this project"}
		}
		if f.ActorPermission < p.MinInputPermission {
			return Decision{Ignored: "actor may not start work"}
		}
		t := cloneTask(core.Task{
			ID: TaskID(ev.Project, ev.Subject), Project: ev.Project, Kind: core.TaskIssue, Subject: ev.Subject,
			State: core.StateDiscovered, Title: ev.Issue.Title, RequirementsHash: ev.Issue.RequirementsHash(), CreatedAt: f.Now,
		})
		m := &machine{t: t, from: core.StateDiscovered, ev: ev, f: f, p: p}
		m.enqueuePlan("new issue")
		m.moveTo(core.StateInvestigating, "new issue accepted")
		d := m.decision()
		d.IsNew = true
		return d
	case core.EventPROpened, core.EventPRReopened:
		if !p.ReviewPullRequests || ev.PullRequest == nil {
			return Decision{Ignored: "review-only workflow not enabled"}
		}
		if f.ActorPermission < p.MinInputPermission {
			return Decision{Ignored: "actor may not start work"}
		}
		t := cloneTask(core.Task{
			ID: TaskID(ev.Project, ev.Subject), Project: ev.Project, Kind: core.TaskReviewOnly, Subject: ev.Subject,
			State: core.StateDiscovered, Title: ev.PullRequest.Title, PullRequest: ev.PullRequest, HeadSHA: ev.PullRequest.HeadSHA, CreatedAt: f.Now,
		})
		m := &machine{t: t, from: core.StateDiscovered, ev: ev, f: f, p: p}
		m.enqueueMain(JobReview, ReviewPayload{SHA: ev.PullRequest.HeadSHA},
			cmp.Or(p.ReviewerLimitKey, p.ImplementerLimitKey), 3)
		m.moveTo(core.StateReviewing, "pull request accepted for review")
		d := m.decision()
		d.IsNew = true
		return d
	}
	return Decision{Ignored: "event does not start work"}
}

type machine struct {
	t       core.Task
	from    core.State
	ev      core.Event
	f       Facts
	p       Policy
	reason  string
	design  *core.Design
	jobs    []core.JobDraft
	cancel  []core.JobKind
	changed bool
	ignored string
}

func (m *machine) decision() Decision {
	if !m.changed {
		return Decision{Ignored: m.ignored}
	}
	t := m.t
	return Decision{Task: &t, From: m.from, Reason: m.reason, Design: m.design, Jobs: m.jobs, CancelKinds: m.cancel}
}

func (m *machine) ignore(why string) { m.ignored = why }

func (m *machine) moveTo(s core.State, reason string) {
	m.t.State = s
	m.reason = reason
	m.changed = true
}

// nextKey returns a unique, deterministic idempotency key for a job of the task.
func (m *machine) nextKey(kind core.JobKind) string {
	m.t.Loops["seq"]++
	return fmt.Sprintf("%s/%s/%d", m.t.ID, kind, m.t.Loops["seq"])
}

func (m *machine) enqueueMain(kind core.JobKind, payload any, limitKey string, attempts int) {
	d := core.JobDraft{Kind: kind, IdempotencyKey: m.nextKey(kind), Payload: mustJSON(payload), LimitKey: limitKey, MaxAttempts: attempts}
	m.t.CurrentJob = &d
	m.jobs = append(m.jobs, d)
	m.changed = true
}

func (m *machine) enqueuePlan(trigger string) {
	m.t.Loops["plan"]++
	m.enqueueMain(JobPlan, PlanPayload{Trigger: trigger}, m.p.PlannerLimitKey, 3)
}

// comment enqueues a message and returns its job key.
func (m *machine) comment(body string) string {
	key := m.nextKey(JobComment)
	m.jobs = append(m.jobs, core.JobDraft{Kind: JobComment, IdempotencyKey: key, Payload: mustJSON(CommentPayload{Body: body}), Priority: 10, MaxAttempts: 10})
	m.changed = true
	return key
}

func (m *machine) notify(sev core.Severity, title, body string) {
	m.jobs = append(m.jobs, core.JobDraft{Kind: JobNotify, IdempotencyKey: m.nextKey(JobNotify), Payload: mustJSON(NotifyPayload{Severity: sev, Title: title, Body: body}), Priority: 10, MaxAttempts: 5})
	m.changed = true
}

// interrupt leaves the current state for blocked or quota-paused, remembering how
// to come back.
func (m *machine) interrupt(to core.State, reason string, until time.Time) {
	if m.t.State != core.StateBlocked && m.t.State != core.StateQuotaPaused {
		m.t.ResumeState = m.t.State
	}
	m.t.Reason = reason
	m.t.PausedUntil = until
	m.moveTo(to, reason)
}

func (m *machine) block(reason string) { m.blockWithDetail(reason, "") }

// blockWithDetail adds detail to the ticket comment only: the reason is also stored on
// the task and sent in notifications, which stay short.
func (m *machine) blockWithDetail(reason, detail string) {
	m.interrupt(core.StateBlocked, reason, time.Time{})
	if detail != "" {
		detail += "\n\n"
	}
	m.comment("Goblins is blocked: " + reason + "\n\n" + detail + "Fix the cause, then a maintainer can comment `/goblins resume`.")
	m.notify(core.SeverityAction, "Task blocked", reason)
}

func (m *machine) fail(reason string) {
	m.t.Reason = reason
	m.t.CurrentJob = nil
	m.t.PausedUntil = time.Time{}
	m.moveTo(core.StateFailed, reason)
	m.comment("Goblins stopped working on this: " + reason)
	m.notify(core.SeverityError, "Task failed", reason)
}

// maxManualInput bounds what a task stores of its manual conversation.
const maxManualInput = 100

// Canonical commands. Each must be alone on the first non-blank line of a new
// comment, which rules out quoted replies and prose that merely mentions them.
var (
	approveArgs = regexp.MustCompile(`^(\d+)(?:\s+([0-9a-f]{8,64}))?$`)
	opCommand   = regexp.MustCompile(`^/goblins\s+(pause|resume|cancel)$`)
)

func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if l := strings.TrimSpace(line); l != "" {
			return l
		}
	}
	return ""
}

func (m *machine) step() {
	switch m.ev.Type {
	case core.EventCommentCreated, core.EventCommentEdited:
		m.onComment()
	case core.EventIssueEdited:
		m.onIssueEdited()
	case core.EventIssueClosed:
		m.cancelTask("the item was closed")
	case core.EventJobStarted:
		m.onJobStarted()
	case core.EventJobFinished:
		m.onJobFinished()
	case core.EventTimerFired:
		if m.t.State == core.StateQuotaPaused && !m.t.PausedUntil.IsZero() && !m.f.Now.Before(m.t.PausedUntil) {
			m.resume("pause time elapsed")
		} else {
			m.ignore("timer no longer applies")
		}
	default:
		m.ignore("event has no effect in this state")
	}
}

func (m *machine) cancelTask(reason string) {
	m.t.CurrentJob = nil
	m.t.PausedUntil = time.Time{}
	m.t.Reason = reason
	m.cancel = slices.Clone(mainKinds)
	m.moveTo(core.StateCancelled, reason)
	m.jobs = append(m.jobs, core.JobDraft{Kind: JobCleanup, IdempotencyKey: m.nextKey(JobCleanup), MaxAttempts: 5})
}

func (m *machine) resume(reason string) {
	to := m.t.ResumeState
	if to == "" {
		m.ignore("nothing to resume")
		return
	}
	m.t.ResumeState = ""
	m.t.PausedUntil = time.Time{}
	m.t.Reason = ""
	switch cur := m.t.CurrentJob; {
	case cur != nil:
		// A fresh key, because the interrupted job's key is already used. The job
		// handler resumes from the stored session rather than starting over.
		d := *cur
		d.IdempotencyKey = m.nextKey(d.Kind)
		m.t.CurrentJob = &d
		m.jobs = append(m.jobs, d)
	case to == core.StateInvestigating:
		m.enqueuePlan(reason)
	case to.IsWaiting():
		// Idle by nature: the next step is a person's.
	default:
		// A working state with no work would hang silently forever.
		m.fail("there is nothing to resume in state " + string(to))
		return
	}
	m.moveTo(to, reason)
}

func (m *machine) onComment() {
	c := m.ev.Comment
	if c == nil || !isHuman(c.Author, m.p) {
		m.ignore("comment by automation")
		return
	}
	line := firstLine(c.Body)
	created := m.ev.Type == core.EventCommentCreated
	if created && opCommand.MatchString(line) {
		m.onOperatorCommand(opCommand.FindStringSubmatch(line)[1], c)
		return
	}
	if created && (line == m.p.ApproveCommand || strings.HasPrefix(line, m.p.ApproveCommand+" ")) {
		m.onApproval(strings.TrimSpace(strings.TrimPrefix(line, m.p.ApproveCommand)), c)
		return
	}
	if m.f.ActorPermission < m.p.MinInputPermission {
		m.ignore("commenter lacks permission to give input")
		return
	}
	switch m.t.State {
	case core.StateWaitingForUser, core.StateDesignProposed:
		// Input older than the current proposal was already seen by the planner.
		if m.t.State == core.StateDesignProposed && c.CreatedAt.Before(m.t.DesignProposedAt) && created {
			m.ignore("comment predates the current design")
			return
		}
		m.recordManualInput(c)
		m.replan("new input from " + c.Author.Login)
	case core.StateInvestigating:
		m.recordManualInput(c)
		m.t.PendingInput = true
		m.changed = true
		m.reason = "input arrived during investigation"
	default:
		m.ignore("input is not requested in this state")
	}
}

// recordManualInput keeps a reply the tracker will never list.
func (m *machine) recordManualInput(c *core.Comment) {
	if m.ev.Source != SourceOperator {
		return
	}
	m.t.ManualInput = append(slices.Clone(m.t.ManualInput), *c)
	if extra := len(m.t.ManualInput) - maxManualInput; extra > 0 {
		m.t.ManualInput = m.t.ManualInput[extra:]
	}
}

func (m *machine) replan(trigger string) {
	if m.t.Loops["plan"] >= m.p.MaxPlanLoops {
		m.block(fmt.Sprintf("the design conversation reached its limit of %d planning rounds", m.p.MaxPlanLoops))
		return
	}
	m.enqueuePlan(trigger)
	m.moveTo(core.StateInvestigating, trigger)
}

func (m *machine) onOperatorCommand(cmd string, c *core.Comment) {
	if m.f.ActorPermission < m.p.MinApprovePermission {
		m.reject(c, fmt.Sprintf("@%s, `/goblins %s` needs %s access.", c.Author.Login, cmd, m.p.MinApprovePermission))
		return
	}
	switch cmd {
	case "cancel":
		m.cancelTask("cancelled by " + c.Author.Login)
		m.comment("Cancelled by @" + c.Author.Login + ".")
	case "pause":
		if m.t.State == core.StateBlocked || m.t.State == core.StateQuotaPaused {
			m.ignore("already paused")
			return
		}
		m.cancel = slices.Clone(mainKinds)
		m.interrupt(core.StateBlocked, "paused by "+c.Author.Login, time.Time{})
	case "resume":
		if m.t.State != core.StateBlocked && m.t.State != core.StateQuotaPaused {
			m.ignore("not paused")
			return
		}
		// A person asked to continue, so bounded loops start over.
		delete(m.t.Loops, "plan")
		delete(m.t.Loops, "implement")
		delete(m.t.Loops, "review_fix")
		m.resume("resumed by " + c.Author.Login)
	}
}

func (m *machine) reject(c *core.Comment, msg string) {
	m.jobs = append(m.jobs, core.JobDraft{Kind: JobComment, IdempotencyKey: m.t.ID + "/reject/" + c.ID, Payload: mustJSON(CommentPayload{Body: msg}), Priority: 10, MaxAttempts: 10})
	m.changed = true
	m.reason = "rejected command: " + msg
}

// onApproval enforces the human gate: a real person with sufficient access, in a
// new comment written after the proposal, naming the exact current revision.
func (m *machine) onApproval(args string, c *core.Comment) {
	if m.f.ActorPermission < m.p.MinApprovePermission {
		m.reject(c, fmt.Sprintf("@%s, approving a design needs %s access. Approval ignored.", c.Author.Login, m.p.MinApprovePermission))
		return
	}
	if m.t.State != core.StateDesignProposed {
		m.reject(c, "There is no design awaiting approval right now, so this approval was ignored.")
		return
	}
	parts := approveArgs.FindStringSubmatch(args)
	if parts == nil {
		m.reject(c, fmt.Sprintf("Approval ignored: use `%s %d`.", m.p.ApproveCommand, m.t.DesignRevision))
		return
	}
	rev, err := strconv.Atoi(parts[1])
	if err != nil || rev != m.t.DesignRevision {
		m.reject(c, fmt.Sprintf("Approval ignored: revision %s is not the current revision %d.", parts[1], m.t.DesignRevision))
		return
	}
	if parts[2] != "" && !strings.HasPrefix(m.t.DesignHash, parts[2]) {
		m.reject(c, "Approval ignored: the hash does not match the current design.")
		return
	}
	if c.CreatedAt.Before(m.t.DesignProposedAt) {
		m.reject(c, fmt.Sprintf("Approval ignored: this comment predates revision %d.", rev))
		return
	}
	m.t.Approval = &core.Approval{Revision: rev, Hash: m.t.DesignHash, Approver: c.Author, CommentID: c.ID, At: c.CreatedAt}
	m.t.Loops["implement"] = 1
	m.enqueueMain(JobImplement, ImplementPayload{Revision: rev, Hash: m.t.DesignHash}, m.p.ImplementerLimitKey, 3)
	m.moveTo(core.StateAgentReady, "design approved by "+c.Author.Login)
	m.comment(fmt.Sprintf("Design revision %d approved by @%s. Implementation is queued.", rev, c.Author.Login))
}

func (m *machine) onIssueEdited() {
	iss := m.ev.Issue
	if iss == nil || !isHuman(m.ev.Actor, m.p) {
		m.ignore("edit by automation")
		return
	}
	if m.f.ActorPermission < m.p.MinInputPermission {
		m.ignore("editor lacks permission to change requirements")
		return
	}
	hash := iss.RequirementsHash()
	if hash == m.t.RequirementsHash {
		m.ignore("requirements unchanged")
		return
	}
	m.t.RequirementsHash = hash
	m.t.Title = iss.Title
	m.changed = true
	m.reason = "requirements changed"
	switch m.t.State {
	case core.StateInvestigating:
		m.t.PendingInput = true
	case core.StateWaitingForUser, core.StateDesignProposed:
		m.replan("requirements changed")
	case core.StateAgentReady, core.StateImplementing:
		// A material change voids approval: the approved design no longer describes
		// what was asked for.
		m.t.Approval = nil
		m.cancel = slices.Clone(mainKinds)
		m.comment("The requirements changed, so the approval of this design no longer applies. Goblins will revise the design and ask again.")
		m.replan("requirements changed after approval")
	case core.StatePROpen, core.StateReviewing, core.StateTesting:
		m.cancel = slices.Clone(mainKinds)
		m.t.Approval = nil
		m.block("the requirements changed after a pull request was opened; a person needs to decide how to proceed")
	default:
		m.reason = "requirements updated while paused"
		m.onEditWhilePaused()
	}
}

// onEditWhilePaused applies the rules of the state the task will return to, so that
// resuming cannot continue work that the edit invalidated.
func (m *machine) onEditWhilePaused() {
	switch m.t.ResumeState {
	case core.StateInvestigating:
		m.t.PendingInput = true
	case core.StateWaitingForUser, core.StateDesignProposed:
		m.replanOnResume()
	case core.StateAgentReady, core.StateImplementing:
		m.t.Approval = nil
		m.cancel = slices.Clone(mainKinds)
		m.comment("The requirements changed, so the approval of this design no longer applies. Goblins will revise the design and ask again once it resumes.")
		m.replanOnResume()
	case core.StatePROpen, core.StateReviewing, core.StateTesting:
		m.t.Approval = nil
		m.cancel = slices.Clone(mainKinds)
		// Blocking also clears any pause timer: this needs a person, not the clock.
		m.block("the requirements changed after a pull request was opened; a person needs to decide how to proceed")
	}
}

// replanOnResume drops the saved job, so that leaving the pause starts a planning round.
func (m *machine) replanOnResume() {
	m.t.CurrentJob = nil
	m.t.ResumeState = core.StateInvestigating
}

func (m *machine) onJobStarted() {
	o := m.ev.Job
	if o == nil || m.t.CurrentJob == nil || o.Key != m.t.CurrentJob.IdempotencyKey {
		m.ignore("not the current job")
		return
	}
	switch {
	case m.t.State == core.StateAgentReady && o.Kind == JobImplement:
		m.moveTo(core.StateImplementing, "implementation started")
	case m.t.State == core.StatePROpen && o.Kind == JobReview:
		m.moveTo(core.StateReviewing, "review started")
	default:
		m.ignore("start needs no state change")
	}
}

func (m *machine) onJobFinished() {
	o := m.ev.Job
	if o == nil {
		m.ignore("no outcome")
		return
	}
	switch o.Kind {
	case JobComment, JobNotify, JobCleanup:
		m.onSideJob(o)
		return
	}
	// Results of superseded or cancelled jobs must never advance the task.
	if m.t.CurrentJob == nil || o.Key != m.t.CurrentJob.IdempotencyKey {
		m.ignore("outcome of a superseded job")
		return
	}
	// A job can finish before the pause that tried to cancel it commits. Resuming runs
	// it again under a fresh key, so dropping this outcome loses no work for good.
	if m.t.State == core.StateBlocked || m.t.State == core.StateQuotaPaused {
		m.ignore("outcome arrived while paused")
		return
	}
	if !o.OK {
		m.onJobFailed(o)
		return
	}
	switch o.Kind {
	case JobPlan:
		m.onPlanDone(o)
	case JobImplement:
		m.onImplementDone(o)
	case JobValidate:
		m.onValidateDone(o)
	case JobPublish:
		m.onPublishDone(o)
	case JobReview:
		m.onReviewDone(o)
	default:
		m.ignore("unknown job kind")
	}
}

func (m *machine) onSideJob(o *core.JobOutcome) {
	if o.OK && o.Kind == JobComment && o.Key == m.t.DesignCommentKey {
		if res, ok := decode[CommentResult](o); ok && !res.PostedAt.IsZero() {
			m.t.DesignProposedAt = res.PostedAt
			m.t.DesignCommentKey = ""
			m.changed = true
			m.reason = "design comment posted"
			return
		}
	}
	if o.OK || o.Kind != JobComment {
		m.ignore("side job finished")
		return
	}
	// No channel reached the user, so waiting for an answer would be silent forever.
	// An explicit, visible state is the only honest outcome.
	if !m.t.State.IsWaiting() {
		m.ignore("comment failed while no user input is awaited: " + o.Message)
		return
	}
	m.t.CurrentJob = nil
	m.t.ResumeState = m.t.State
	m.t.Reason = "cannot reach the user on the tracker: " + o.Message
	m.moveTo(core.StateBlocked, m.t.Reason)
	m.notify(core.SeverityAction, "Task blocked", m.t.Reason)
}

func (m *machine) onJobFailed(o *core.JobOutcome) {
	msg := o.Message
	if msg == "" {
		msg = string(o.ErrorKind)
	}
	switch o.ErrorKind {
	case core.KindQuotaExhausted:
		m.interrupt(core.StateQuotaPaused, "agent usage limit reached: "+msg, m.f.Now.Add(m.p.QuotaPause))
		m.notify(core.SeverityInfo, "Task paused: usage limit", msg)
	case core.KindAuthRequired:
		m.interrupt(core.StateQuotaPaused, "agent login is required: "+msg, m.f.Now.Add(m.p.AuthPause))
		m.notify(core.SeverityAction, "Agent login required", msg)
		m.comment("Goblins paused: the agent needs a person to log in again on the installation. Work resumes automatically afterwards.")
	case core.KindUnsupported, core.KindForbidden, core.KindUnauthorized, core.KindInvalid, core.KindNotFound:
		m.block(msg)
	case core.KindCancelled:
		m.ignore("job cancelled")
	default:
		m.fail(fmt.Sprintf("%s job failed: %s", o.Kind, msg))
	}
}

func decode[T any](o *core.JobOutcome) (T, bool) {
	var v T
	if err := json.Unmarshal(o.Result, &v); err != nil {
		return v, false
	}
	return v, true
}

func (m *machine) onPlanDone(o *core.JobOutcome) {
	res, ok := decode[PlanResult](o)
	if !ok || (len(res.Questions) == 0) == (res.Design == "") {
		m.block("the planner returned a result that is neither questions nor a design")
		return
	}
	if res.SessionID != "" {
		m.t.Sessions[core.RolePlanner] = res.SessionID
	}
	m.t.CurrentJob = nil
	if m.t.PendingInput {
		// Newer input exists, so this result is stale: run again instead of posting it.
		m.t.PendingInput = false
		m.replan("input arrived during investigation")
		return
	}
	if len(res.Questions) > 0 {
		m.comment(RenderQuestions(res.Questions))
		m.moveTo(core.StateWaitingForUser, "planner has questions")
		return
	}
	hash := core.Digest(res.Design)
	if hash != m.t.DesignHash {
		m.t.DesignRevision++
		m.t.DesignHash = hash
		m.t.Approval = nil
		m.design = &core.Design{TaskID: m.t.ID, Revision: m.t.DesignRevision, Hash: hash, Body: res.Design, CreatedAt: m.f.Now}
	}
	// Approvals must come after this moment. Once the comment is posted, this is replaced
	// by the tracker's own timestamp for it, the same clock replies are stamped with.
	m.t.DesignProposedAt = m.f.Now
	m.t.DesignCommentKey = m.comment(RenderDesign(m.t.DesignRevision, hash, res.Design, m.p.ApproveCommand))
	m.moveTo(core.StateDesignProposed, fmt.Sprintf("design revision %d proposed", m.t.DesignRevision))
}

func (m *machine) onImplementDone(o *core.JobOutcome) {
	res, ok := decode[ImplementResult](o)
	if !ok {
		m.block("the implementer returned an unreadable result")
		return
	}
	if res.SessionID != "" {
		m.t.Sessions[core.RoleImplementer] = res.SessionID
	}
	var p ImplementPayload
	_ = json.Unmarshal(m.t.CurrentJob.Payload, &p)
	// A correction starts from a commit, so the base comparison would always see a change.
	unchanged := p.ReviewFeedback != "" && res.HeadSHA == m.t.HeadSHA
	m.t.Workspace, m.t.Branch, m.t.BaseSHA = res.Workspace, res.Branch, res.BaseSHA
	m.t.HeadSHA = res.HeadSHA
	if !res.Changed || unchanged {
		m.block("the implementer finished without changing anything")
		return
	}
	m.enqueueMain(JobValidate, ValidatePayload{Stage: StagePrePR, SHA: res.HeadSHA}, "", 3)
	m.reason = "implementation produced " + res.HeadSHA
	if m.t.State == core.StateAgentReady {
		m.moveTo(core.StateImplementing, m.reason)
	}
}

func (m *machine) onValidateDone(o *core.JobOutcome) {
	res, ok := decode[ValidateResult](o)
	if !ok {
		m.block("validation returned an unreadable result")
		return
	}
	rep := res.Report
	if rep.Status == core.ValidationError {
		m.block("validation could not run: " + rep.Summary)
		return
	}
	var p ValidatePayload
	_ = json.Unmarshal(m.t.CurrentJob.Payload, &p)
	skipped := res.Skipped && rep.Status == core.ValidationSkipped
	if rep.Status != core.ValidationPassed && !skipped {
		m.onValidationFailed(p, rep)
		return
	}
	if !skipped {
		m.t.ValidatedSHA = p.SHA // only a pass records this
	}
	if p.Stage == StagePrePR {
		m.enqueuePublish(p.SHA)
		m.reason = "validation passed before publishing"
		if skipped {
			m.reason = "no validation configured, publishing an untested commit as allowed"
		}
		return
	}
	m.t.CurrentJob = nil
	note := ""
	if res.Skipped {
		note = "\n\nNote: this project configures no validation, so nothing was tested."
	}
	m.comment(RenderReport(&m.t, rep) + note)
	m.finish("validation passed")
}

func (m *machine) onValidationFailed(p ValidatePayload, rep core.ValidationReport) {
	if p.Stage != StagePrePR {
		m.blockWithDetail("validation failed on the pull request: "+rep.Summary, RenderFailedChecks(rep))
		return
	}
	retry := ImplementPayload{Revision: m.t.DesignRevision, Hash: m.t.DesignHash,
		Feedback: RenderFailure(rep), ReviewFeedback: m.t.CorrectionFeedback}
	if m.t.Loops["implement"] >= m.p.MaxImplementLoops {
		// Not enqueued, but recorded: if a person resumes, this is the work to do.
		m.t.CurrentJob = &core.JobDraft{Kind: JobImplement, IdempotencyKey: m.nextKey(JobImplement), Payload: mustJSON(retry), LimitKey: m.p.ImplementerLimitKey, MaxAttempts: 3}
		m.blockWithDetail(fmt.Sprintf("validation still fails after %d implementation rounds: %s", m.p.MaxImplementLoops, rep.Summary), RenderFailedChecks(rep))
		return
	}
	m.t.Loops["implement"]++
	m.enqueueMain(JobImplement, retry, m.p.ImplementerLimitKey, 3)
	m.reason = "validation failed, another implementation round"
}
