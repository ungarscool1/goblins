package pipeline

import (
	"context"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/scheduler"
	"github.com/ungarscool1/goblins/internal/workflow"
)

// status posts the task's status comment once and edits it from then on. Its idempotency key
// never changes, so a retry, a restart or a lost ID all find the same comment instead of
// posting another. A tracker that cannot edit gets a new comment per distinct status, and one
// that cannot take comments gets none: progress needs nobody's attention, and what does is
// sent as a message of its own.
func (p *Pipeline) status(ctx context.Context, jc scheduler.JobContext) (any, error) {
	pr, err := p.project(jc.Task.Project)
	if err != nil {
		return nil, err
	}
	payload, err := decodePayload[workflow.StatusPayload](jc.Job)
	if err != nil {
		return nil, err
	}
	info := pr.Tracker.Info()
	if jc.Task.Subject.Kind != core.SubjectIssue || !info.Has(core.CapTrackerCommentWrite) {
		return workflow.StatusResult{}, nil
	}
	ref := issueRef(jc.Task)
	if !info.Has(core.CapTrackerCommentEdit) {
		c, err := pr.Tracker.PostComment(ctx, ref, core.CommentDraft{
			IdempotencyKey: jc.Task.ID + "/status/" + core.Digest(payload.Body)[:16],
			Body:           payload.Body})
		return workflow.StatusResult{CommentID: c.ID}, err
	}
	draft := core.CommentDraft{IdempotencyKey: jc.Task.ID + "/status", Body: payload.Body}
	if id := jc.Task.Status.CommentID; id != "" {
		c, err := pr.Tracker.UpdateComment(ctx, ref, id, draft)
		if !core.IsKind(err, core.KindNotFound) {
			return workflow.StatusResult{CommentID: c.ID}, err
		}
		jc.Log("system", "the status comment was deleted, so it is posted again")
	}
	c, err := pr.Tracker.PostComment(ctx, ref, draft)
	if err != nil {
		return nil, err
	}
	// The key may have found what an interrupted attempt posted, with an older text.
	c, err = pr.Tracker.UpdateComment(ctx, ref, c.ID, draft)
	return workflow.StatusResult{CommentID: c.ID}, err
}
