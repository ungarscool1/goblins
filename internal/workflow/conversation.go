package workflow

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

const (
	maxFindingHistory = 30
	maxReplyBytes     = 2 << 10
)

// reopensOnReply says whether a new remark needs judging again. Only a person's words do when
// waiting for a reply, so that two automated reviewers can never keep each other busy.
func reopensOnReply(status core.FindingStatus, discussion []core.DiscussionEntry) bool {
	if len(discussion) == 0 {
		return false
	}
	last := discussion[len(discussion)-1]
	if last.Own {
		return false
	}
	switch status {
	case core.FindingAwaitingReviewer, core.FindingDisputed, core.FindingFixedPending:
		return last.Human
	case core.FindingNeedsFix, core.FindingAddressed:
		return last.Human
	}
	return false
}

func (m *machine) setStatus(f *core.ReviewFinding, status core.FindingStatus, note string) {
	f.Status = status
	f.History = append(f.History, core.FindingEvent{At: m.f.Now, Status: status, SHA: m.t.HeadSHA,
		Note: capNote(note)})
	if extra := len(f.History) - maxFindingHistory; extra > 0 {
		f.History = f.History[extra:]
	}
	m.changed = true
}

func capNote(s string) string {
	if len(s) <= 500 {
		return s
	}
	cut := strings.ToValidUTF8(s[:500], "")
	if i := strings.LastIndexAny(cut, " \n"); i > 0 {
		cut = cut[:i] // a word cut in half reads as a mistake
	}
	return cut + "..."
}

// converse starts judging what is new. It runs no agent for what needs none: a change request
// with nothing in it can only be asked about. It reports whether a job was queued.
func (m *machine) converse(res ReconcileResult) bool {
	var open []string
	for i := range m.t.Findings {
		f := &m.t.Findings[i]
		if f.Status != core.FindingOpen {
			continue
		}
		if f.Body == "" && len(f.Discussion) == 0 {
			m.askForContent(f)
			continue
		}
		open = append(open, f.ID)
	}
	if len(open) == 0 {
		return false
	}
	if m.t.Loops["triage"] >= m.p.MaxTriageRounds {
		m.block(fmt.Sprintf("the review conversation reached its limit of %d rounds; a person needs to decide how to proceed", m.p.MaxTriageRounds))
		return true
	}
	m.t.Loops["triage"]++
	payload := TriagePayload{SHA: m.t.HeadSHA, Findings: open, Checks: res.Checks}
	m.enqueueMain(JobTriage, payload, m.p.ImplementerLimitKey, 3)
	m.reason = fmt.Sprintf("judging %d findings on %s", len(open), m.t.HeadSHA)
	return true
}

// askForContent answers a change request that says nothing. It is a real gate, and it stays one.
func (m *machine) askForContent(f *core.ReviewFinding) {
	m.setStatus(f, core.FindingAwaitingReviewer, "change request with no content")
	m.reply(f,
		fmt.Sprintf("@%s, you requested changes without saying which. What would you like to see different? "+
			"Nothing changes here until then, and your review keeps standing until you clear it.",
			f.Author.Login))
	m.notify(core.SeverityAction, "Change request without content",
		fmt.Sprintf("@%s requested changes with no remark.", f.Author.Login))
	m.waitForReply()
}

// reply queues an answer in the finding's conversation. The key depends on what is being
// answered, so that neither a retried job nor a replayed event says it twice. Anything a model
// wrote must have been through cleanReply.
func (m *machine) reply(f *core.ReviewFinding, body string) {
	key := fmt.Sprintf("%s/reply/%s/%s/%d", m.t.ID, f.ID, m.t.HeadSHA, f.DebateRounds)
	if f.Status == core.FindingFixedPending {
		key = fmt.Sprintf("%s/reply/%s/fix/%s", m.t.ID, f.ID, f.FixSHA)
	}
	payload := ReplyPayload{Finding: f.ID, Thread: f.ThreadID, Key: key, Body: body}
	m.jobs = append(m.jobs, core.JobDraft{Kind: JobReply, IdempotencyKey: m.nextKey(JobReply),
		Payload: mustJSON(payload), Priority: 10, MaxAttempts: 10})
	m.changed = true
}

// cleanReply keeps model output from formatting a thread beyond plain remarks, mentioning
// anyone or carrying markers. It is posted in public, and bounded.
func cleanReply(s string) string {
	s = strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "<!--", "<! --")
	s = strings.ReplaceAll(s, "@", "@\u200b")
	if len(s) > maxReplyBytes {
		s = strings.ToValidUTF8(s[:maxReplyBytes], "") + "..."
	}
	return strings.TrimSpace(s)
}

func (m *machine) waitForReply() {
	if m.p.ReplyTimeout > 0 {
		m.t.PausedUntil = m.f.Now.Add(m.p.ReplyTimeout)
	}
}

func (m *machine) finding(id string) *core.ReviewFinding {
	i := slices.IndexFunc(m.t.Findings, func(f core.ReviewFinding) bool { return f.ID == id })
	if i < 0 {
		return nil
	}
	return &m.t.Findings[i]
}

// onTriageDone applies judgements. A model only ever picks among these outcomes: what it says
// about a finding cannot approve anything, resolve a thread or widen what may be changed.
func (m *machine) onTriageDone(o *core.JobOutcome) {
	res, ok := decode[TriageResult](o)
	if !ok {
		m.block("judging the review findings returned an unreadable result")
		return
	}
	if res.SessionID != "" {
		m.t.Sessions[core.RoleTriager] = res.SessionID
	}
	var payload TriagePayload
	_ = json.Unmarshal(m.t.CurrentJob.Payload, &payload)
	m.t.CurrentJob = nil
	var fixes []core.ReviewFinding
	var conflicts []string
	escalate := ""
	for _, item := range res.Items {
		f := m.finding(item.ID)
		if f == nil || !slices.Contains(payload.Findings, item.ID) || f.Status != core.FindingOpen {
			continue // the model named something it was not asked about
		}
		f.Plan, f.Evidence = capNote(item.Plan), capNote(item.Evidence)
		switch item.Decision {
		case TriageValid:
			m.setStatus(f, core.FindingNeedsFix, "judged valid: "+item.Plan)
			fixes = append(fixes, *f)
		case TriageSuperseded:
			m.setStatus(f, core.FindingSuperseded, item.Evidence)
			if f.Author.Type == core.ActorHuman {
				m.reply(f, cleanReply("This no longer seems to apply: "+item.Evidence+
					"\n\nIf I have this wrong, please say so here."))
			}
		case TriageUnclear, TriageDisputed:
			if why := m.debate(f, item); why != "" {
				escalate = why
			}
		case TriageDesignChange:
			m.setStatus(f, core.FindingDisputed, "needs a design change: "+item.Evidence)
			m.reply(f, cleanReply("Doing this would change what the approved design says, so it "+
				"needs a new design approval first: "+item.Evidence))
			m.voidDesign(f)
			return
		case TriageConflict:
			m.setStatus(f, core.FindingDisputed, "reviewers disagree: "+item.Evidence)
			conflicts = append(conflicts, f.ID)
		default:
			m.setStatus(f, core.FindingOpen, "the judgement named an unknown decision")
		}
	}
	switch {
	case len(conflicts) > 0:
		m.block("reviewers disagree about this pull request, and a person needs to decide")
	case escalate != "":
		m.block(escalate)
	case len(fixes) > 0:
		m.sendBack("the review", RenderTriagedFeedback(fixes, m.t.HeadSHA), fixes, m.t.HeadSHA)
	default:
		m.enqueueReconcile()
	}
}

// debate posts a question or an objection, within bounds. Waiting for the reply costs nothing.
func (m *machine) debate(f *core.ReviewFinding, item TriageItem) (escalate string) {
	limit := m.p.MaxDebateRounds
	if f.Author.Type != core.ActorHuman {
		limit = 1 // no arguing with automation
	}
	status := core.FindingAwaitingReviewer
	if item.Decision == TriageDisputed {
		status = core.FindingDisputed
	}
	if f.DebateRounds >= limit {
		m.setStatus(f, status, "discussion limit reached")
		return fmt.Sprintf("the discussion of a finding at %s reached its limit of %d rounds without agreement, and a person needs to decide", findingLocation(*f), limit)
	}
	f.DebateRounds++
	m.setStatus(f, status, item.Evidence)
	body := strings.TrimSpace(item.Reply)
	if item.Decision == TriageDisputed {
		body += "\n\nCould you confirm, or propose an alternative? I have not changed the code for this."
	}
	m.reply(f, cleanReply(body))
	m.waitForReply()
	return ""
}

// voidDesign sends a material change back through design approval: nothing more gets
// implemented without a person approving a new revision.
func (m *machine) voidDesign(f *core.ReviewFinding) {
	m.t.Approval = nil
	m.t.ManualInput = append(slices.Clone(m.t.ManualInput), core.Comment{ID: "finding-" + f.ID,
		Author: f.Author, Body: f.Body, CreatedAt: m.f.Now})
	if extra := len(m.t.ManualInput) - maxManualInput; extra > 0 {
		m.t.ManualInput = m.t.ManualInput[extra:]
	}
	m.block("a review finding needs a change to the approved design, which voids its approval. Resuming revises the design, and it needs approving again")
	m.replanOnResume()
}

// confirmFixes reports on what a correction did, in the threads that asked for it, linked to
// the commit and what checked it. It never says a finding is resolved: a push proves nothing,
// and the thread stays open for its reviewer.
func (m *machine) confirmFixes() {
	for i := range m.t.Findings {
		f := &m.t.Findings[i]
		if f.Status != core.FindingNeedsFix {
			continue
		}
		f.FixSHA = m.t.HeadSHA
		m.setStatus(f, core.FindingFixedPending, "correction in "+m.t.HeadSHA)
		if !m.p.SkipReviewTriage {
			m.reply(f, cleanReply(fixReply(*f, m.t.ValidatedSHA == m.t.HeadSHA)))
		}
	}
}

func fixReply(f core.ReviewFinding, validated bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Fixed in `%s`.", f.FixSHA)
	if validated {
		b.WriteString("\n\nThe project's checks passed on that commit.")
	}
	if f.Evidence != "" {
		b.WriteString("\n\nWhy I agreed: " + f.Evidence)
	}
	b.WriteString("\n\nThis is yours to confirm: I have left the thread open.")
	return b.String()
}

// onReplyDone handles the outcome of an answer. Not being able to answer in a thread must not
// look like waiting for a reply nobody was asked for.
func (m *machine) onReplyDone(o *core.JobOutcome) {
	if o.OK {
		m.ignore("reply posted")
		return
	}
	if m.t.State == core.StateBlocked || Terminal(m.t.State) {
		m.ignore("reply failed while nobody is waiting on it: " + o.Message)
		return
	}
	m.t.CurrentJob = nil
	m.block("Goblins could not answer in a review thread: " + o.Message)
}

// onReplyTimeout tells a person that a question went unanswered. It keeps waiting: the reviewer
// can still answer at any time.
func (m *machine) onReplyTimeout() {
	m.t.PausedUntil = time.Time{}
	m.changed = true
	var waiting []string
	for _, f := range m.t.Findings {
		if f.Status == core.FindingAwaitingReviewer || f.Status == core.FindingDisputed {
			waiting = append(waiting, fmt.Sprintf("- %s, from @%s", findingLocation(f),
				f.Author.Login))
		}
	}
	if len(waiting) == 0 {
		m.reason = "no reply is awaited"
		return
	}
	body := "No reply yet on these review questions, so a person may need to step in:\n\n" + strings.Join(waiting, "\n")
	m.comment(body)
	m.notify(core.SeverityAction, "Review questions unanswered", body)
	m.reason = "a review question went unanswered"
}
