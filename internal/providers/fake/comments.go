package fake

import (
	"context"
	"maps"
	"slices"

	"github.com/ungarscool1/goblins/internal/core"
)

func (t *Tracker) UpdateComment(_ context.Context, ref core.IssueRef, id string,
	d core.CommentDraft) (core.Comment, error) {
	if !t.Info().Has(core.CapTrackerCommentEdit) {
		return core.Comment{}, core.Unsupported(t.Info(), core.CapTrackerCommentEdit)
	}
	t.W.mu.Lock()
	defer t.W.mu.Unlock()
	comments := t.W.comments[refKey(ref)]
	i := slices.IndexFunc(comments, func(c core.Comment) bool { return c.ID == id })
	if i < 0 {
		return core.Comment{}, core.Errorf(core.KindNotFound, "fake.update_comment",
			"comment %s not found", id)
	}
	if comments[i].Author.ID != t.W.Bot.ID {
		return core.Comment{}, core.Errorf(core.KindForbidden, "fake.update_comment",
			"comment %s is not ours", id)
	}
	if comments[i].Body != d.Body {
		comments[i].Body, comments[i].UpdatedAt = d.Body, t.W.Now()
		t.W.edits++
	}
	c := comments[i]
	if d.IdempotencyKey != "" {
		t.W.byKey[refKey(ref)+"|"+d.IdempotencyKey] = c
	}
	c.Own = true
	return c, nil
}

// DeleteComment removes a comment, as a person with access can.
func (w *World) DeleteComment(ref core.IssueRef, id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.comments[refKey(ref)] = slices.DeleteFunc(w.comments[refKey(ref)],
		func(c core.Comment) bool { return c.ID == id })
	maps.DeleteFunc(w.byKey, func(_ string, c core.Comment) bool { return c.ID == id })
}

// Edits counts the comments the integration changed in place.
func (w *World) Edits() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.edits
}
