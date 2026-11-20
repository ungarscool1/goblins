package console

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/workflow"
)

// Actions an operator can take. Pausing marks a task blocked, and resuming runs the
// interrupted step again.
const (
	actAnswer  = "answer"
	actApprove = "approve"
	actPause   = "pause"
	actResume  = "resume"
	actCancel  = "cancel"
)

// Action is what an operator asks for. Text is an answer; Revision and Hash name the design
// being approved, so that a newer revision is never approved by mistake.
type Action struct {
	Action   string `json:"action"`
	Text     string `json:"text,omitempty"`
	Revision int    `json:"revision,omitempty"`
	Hash     string `json:"hash,omitempty"`
}

var hexHash = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

// act enters an action as an operator comment. It returns once the event is stored: the
// workflow then applies it, or refuses it, as it would the same comment on the ticket.
func (c *console) act(ctx context.Context, op *Operator, id string, a Action) error {
	if op == nil || !op.CanAct || c.o.ReadOnly {
		return core.Errorf(core.KindForbidden, "console.act",
			"this console or operator is read-only")
	}
	t, err := c.o.Store.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if t.State.IsTerminal() {
		return core.Errorf(core.KindConflict, "console.act", "the task is %s", t.State)
	}
	body, err := c.command(t, a)
	if err != nil {
		return err
	}
	if _, _, err := c.o.Store.AppendEvent(ctx,
		workflow.OperatorComment(t, op.Name, body, c.o.Now().UTC())); err != nil {
		return err
	}
	c.o.Log.Info("console action", "operator", op.Name, "task", t.ID, "action", a.Action)
	return nil
}

// command words an action as the comment that does it.
func (c *console) command(t core.Task, a Action) (string, error) {
	switch a.Action {
	case actPause, actResume, actCancel:
		return "/goblins " + a.Action, nil
	case actApprove:
		pol, ok := c.o.Policies(t.Project)
		if !ok {
			return "", core.Errorf(core.KindNotFound, "console.act", "unknown project %s",
				t.Project)
		}
		if a.Revision < 1 || !hexHash.MatchString(a.Hash) {
			return "", core.Errorf(core.KindInvalid, "console.act",
				"an approval needs the design's revision and hash")
		}
		return fmt.Sprintf("%s %d %s", pol.ApproveCommand, a.Revision, a.Hash), nil
	case actAnswer:
		text := strings.TrimSpace(a.Text)
		if text == "" {
			return "", core.Errorf(core.KindInvalid, "console.act", "the answer is empty")
		}
		// Commands have actions of their own, so an answer never turns into one.
		if strings.HasPrefix(text, "/") {
			return "", core.Errorf(core.KindInvalid, "console.act",
				"an answer cannot start with /, use the actions instead")
		}
		return text, nil
	}
	return "", core.Errorf(core.KindInvalid, "console.act", "unknown action %q", a.Action)
}
