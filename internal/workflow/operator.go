package workflow

import (
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
