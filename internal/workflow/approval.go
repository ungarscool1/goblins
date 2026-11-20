package workflow

import (
	"fmt"
	"strings"

	"github.com/ungarscool1/goblins/internal/core"
)

// BlueprintVersion names the blueprint this code runs: its typed stages and their order, as
// docs/blueprints.md describes them. It is recorded with what executes each stage.
const BlueprintVersion = "issue-to-draft@1"

// ApprovalProfile says when a design may go to implementation without a person approving it.
// The default profile is strict: every design waits for a person. An unattended profile is an
// operator's explicit choice for a constrained scope, and a person still invokes it, per item,
// with a label: the model never decides that something is safe.
type ApprovalProfile struct {
	Unattended bool
	// Label is what a person with approval permission applies to run an item unattended.
	Label string
	// AllowedPaths must match every path a change touches; DeniedPaths none of them. A change
	// outside either stops for a person. The configuration adds its sensitive paths to these.
	AllowedPaths    []string
	DeniedPaths     []string
	MaxChangedFiles int
}

// The transitions approval profiles add to the table: a change leaving the unattended scope
// goes back to waiting for approval. Nothing reaches a worker without passing design-proposed.
func init() {
	for _, from := range []core.State{core.StateAgentReady, core.StateImplementing} {
		transitions[from] = append(transitions[from], core.StateDesignProposed)
	}
}

// Hash names the profile's rules, so an approval records exactly which rules allowed it.
func (a ApprovalProfile) Hash() string {
	return core.Digest(a.Label, strings.Join(a.AllowedPaths, "\n"), strings.Join(a.denied(), "\n"),
		fmt.Sprint(a.MaxChangedFiles))
}

func (a ApprovalProfile) denied() []string { return a.DeniedPaths }

// intakeRefusal says why an event does not start work, or nothing. The unattended label
// starts work too, from someone allowed to approve, as it is the stronger signal.
func intakeRefusal(ev core.Event, f Facts, p Policy) string {
	if unattendedSignal(ev, f, p) {
		return ""
	}
	if p.IntakeLabel != "" && (ev.Type != core.EventIssueLabeled || ev.Label != p.IntakeLabel) {
		return "intake label not applied"
	}
	if p.IntakeLabel == "" && ev.Type != core.EventIssueOpened {
		return "label is not an intake signal for this project"
	}
	return ""
}

func unattendedSignal(ev core.Event, f Facts, p Policy) bool {
	a := p.Approval
	return a.Unattended && a.Label != "" && ev.Type == core.EventIssueLabeled &&
		ev.Label == a.Label && isHuman(ev.Actor, p) && f.ActorPermission >= p.MinApprovePermission
}

func unattendedBy(ev core.Event, f Facts, p Policy) *core.Authorization {
	if !unattendedSignal(ev, f, p) {
		return nil
	}
	return &core.Authorization{Actor: ev.Actor, Label: ev.Label, At: f.Now}
}

// onOtherEvent handles a label that allows an item to run unattended, once it exists.
func (m *machine) onOtherEvent() {
	if !unattendedSignal(m.ev, m.f, m.p) || m.t.Unattended != nil || m.t.Escalated != "" {
		m.ignore("event has no effect in this state")
		return
	}
	m.t.Unattended = unattendedBy(m.ev, m.f, m.p)
	m.reason, m.changed = "allowed to run unattended by "+m.ev.Actor.Login, true
	if m.t.State == core.StateDesignProposed && m.t.Approval == nil && m.t.DesignCommentKey == "" {
		m.approveByPolicy()
	}
}

// designPosted goes on once a design is visible on the ticket. An unattended task approves it
// then, never before: what runs without a person is always on the record first.
func (m *machine) designPosted() {
	m.reason = "design comment posted"
	if m.t.State == core.StateDesignProposed && m.t.Approval == nil && m.t.Unattended != nil &&
		m.p.Approval.Unattended && m.t.Escalated == "" {
		m.approveByPolicy()
	}
}

func (m *machine) approveByPolicy() {
	who := m.t.Unattended.Actor
	m.t.Approval = &core.Approval{Revision: m.t.DesignRevision, Hash: m.t.DesignHash,
		Approver: who, At: m.f.Now, Policy: "unattended@" + m.p.Approval.Hash()[:12]}
	m.t.Loops["implement"] = 1
	m.enqueueMain(JobImplement, ImplementPayload{Revision: m.t.DesignRevision,
		Hash: m.t.DesignHash}, m.p.ImplementerLimitKey, 3)
	m.moveTo(core.StateAgentReady, "design approved by the unattended profile, on "+who.Login+
		"'s label")
	m.comment(fmt.Sprintf("Design revision %d runs unattended, as @%s allowed with the `%s` "+
		"label. If the change touches anything outside this project's unattended scope, it "+
		"stops and waits for `%s %d`.", m.t.DesignRevision, who.Login, m.p.Approval.Label,
		m.p.ApproveCommand, m.t.DesignRevision))
}

// afterApproval goes on from a person's approval: with the change that was waiting for it, if
// one left the unattended scope, or else to implementing the design.
func (m *machine) afterApproval(who string) {
	m.t.Loops["implement"] = 1
	if esc := m.t.Escalated; esc != "" && esc == m.t.HeadSHA {
		m.t.Escalated = ""
		m.enqueueMain(JobValidate, ValidatePayload{Stage: StagePrePR, SHA: esc}, "", 3)
		m.moveTo(core.StateAgentReady, "design approved by "+who+" for the change at "+esc)
		m.progress(fmt.Sprintf("approved by @%s. Checking the change at `%s`.", who, esc))
		return
	}
	m.t.Escalated = ""
	m.enqueueMain(JobImplement, ImplementPayload{Revision: m.t.DesignRevision,
		Hash: m.t.DesignHash}, m.p.ImplementerLimitKey, 3)
	m.moveTo(core.StateAgentReady, "design approved by "+who)
	m.progress(fmt.Sprintf("design revision %d approved by @%s. Implementation is queued.",
		m.t.DesignRevision, who))
}

// outOfScope stops a change approved by policy that touches what the policy does not cover.
// It decides from the paths the commit changed, never from what a model says it did, and from
// then on the task needs a person: running unattended once more would need a new label.
func (m *machine) outOfScope(res ImplementResult) bool {
	a := m.t.Approval
	if a == nil || a.Policy == "" {
		return false
	}
	why := scopeViolation(m.p.Approval, res)
	if why == "" {
		return false
	}
	m.t.Approval, m.t.Unattended, m.t.Escalated, m.t.CurrentJob = nil, nil, res.HeadSHA, nil
	m.t.DesignProposedAt = m.f.Now
	m.moveTo(core.StateDesignProposed, "the change left the unattended scope: "+why)
	// The status comment already shows: it must not go on saying the work is under way.
	m.progress("the change left the unattended scope, and waits for a person's approval")
	m.t.DesignCommentKey = m.comment(fmt.Sprintf("The change at `%s` left this project's "+
		"unattended scope: %s. Nothing was published. To go on with it, approve the design: "+
		"`%s %d`.",
		res.HeadSHA, why, m.p.ApproveCommand, m.t.DesignRevision))
	return true
}

func scopeViolation(a ApprovalProfile, res ImplementResult) string {
	if a.MaxChangedFiles > 0 && res.ChangedFiles > a.MaxChangedFiles {
		return fmt.Sprintf("%d files changed, the scope allows %d", res.ChangedFiles,
			a.MaxChangedFiles)
	}
	if res.ChangedFiles > len(res.ChangedPaths) {
		return "more files changed than can be checked"
	}
	var out []string
	for _, p := range res.ChangedPaths {
		if matchesAny(p, a.denied()) || !matchesAny(p, a.AllowedPaths) {
			out = append(out, "`"+safePath(p)+"`")
		}
		if len(out) == 10 {
			out = append(out, "and more")
			break
		}
	}
	if len(out) == 0 {
		return ""
	}
	return "it changes " + strings.Join(out, ", ")
}

// safePath keeps a path a repository chose from formatting a comment.
func safePath(p string) string {
	return strings.Map(func(r rune) rune {
		if r == '`' || r < ' ' {
			return '?'
		}
		return r
	}, p)
}
