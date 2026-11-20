package workflow

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// OperatorComment is a comment entered by an operator of this installation, from its shell or
// its console. It is the manual inbox for trackers that cannot carry a conversation: the same
// rules apply to its content, and access to the installation is the proof of permission.
func OperatorComment(task core.Task, login, body string, now time.Time) core.Event {
	actor := core.Actor{ID: "operator:" + login, Login: login, Type: core.ActorHuman}
	id := core.Digest(task.ID, login, body, now.String())[:24]
	return core.Event{Project: task.Project, Source: SourceOperator,
		Type: core.EventCommentCreated, Subject: task.Subject, DedupKey: "operator:" + id,
		Actor: actor, OccurredAt: now, ReceivedAt: now,
		Comment: &core.Comment{ID: "operator-" + id, Author: actor, Body: body,
			CreatedAt: now, UpdatedAt: now}}
}

func (m *machine) onOperatorCommand(line string, c *core.Comment) {
	words := strings.Fields(line) // the command, and the commit for deploy
	cmd, arg := words[0], strings.Join(words[1:], " ")
	if m.f.ActorPermission < m.p.MinApprovePermission {
		m.reject(c, fmt.Sprintf("@%s, `/goblins %s` needs %s access.", c.Author.Login, cmd, m.p.MinApprovePermission))
		return
	}
	switch cmd {
	case "deploy":
		m.onDeployApproval(arg, c)
	case "cancel":
		m.cancelTask("cancelled by " + c.Author.Login)
		m.progress("cancelled by @" + c.Author.Login + ".")
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
		if m.awaitingDeploy() {
			m.reject(c, fmt.Sprintf("This task waits for its deployment to be approved: use "+
				"`/goblins deploy %s`.", short(m.t.Deploy.SHA)))
			return
		}
		m.t.TokenBudgetFrom = m.t.TokensUsed // a person let it continue: a new budget
		// A person asked to continue, so bounded loops start over.
		delete(m.t.Loops, "plan")
		delete(m.t.Loops, "implement")
		delete(m.t.Loops, "review_fix")
		delete(m.t.Loops, "triage")
		m.releaseHold()
		m.resume("resumed by " + c.Author.Login)
	}
}

func (m *machine) reject(c *core.Comment, msg string) {
	m.jobs = append(m.jobs, core.JobDraft{Kind: JobComment, IdempotencyKey: m.t.ID + "/reject/" + c.ID, Payload: mustJSON(CommentPayload{Body: msg}), Priority: 10, MaxAttempts: 10})
	m.changed = true
	m.reason = "rejected command: " + msg
}
