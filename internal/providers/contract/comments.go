package contract

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// trackerEdits checks that a tracker edits its own comments in place, keeps their key, and
// never edits a person's.
func trackerEdits(t *testing.T, h TrackerHarness) {
	ctx := context.Background()
	ref := h.Seed(t, "T", "B")
	if !h.Tracker.Info().Has(core.CapTrackerCommentEdit) {
		t.Skip("no comment.edit")
	}
	draft := core.CommentDraft{IdempotencyKey: "status", Body: "first"}
	a, err := h.Tracker.PostComment(ctx, ref, draft)
	if err != nil {
		t.Fatal(err)
	}
	draft.Body = "second"
	if _, err := h.Tracker.UpdateComment(ctx, ref, a.ID, draft); err != nil {
		t.Fatal(err)
	}
	// The key still finds the same comment, which now says the new thing.
	again, err := h.Tracker.PostComment(ctx, ref, draft)
	if err != nil || again.ID != a.ID || !strings.Contains(again.Body, "second") {
		t.Fatalf("after an edit, the key found %+v, %v", again, err)
	}
	all, err := h.Tracker.ListComments(ctx, ref, time.Time{})
	if err != nil || len(all) != 1 {
		t.Fatalf("comments = %d, %v", len(all), err)
	}
	human := h.SeedComment(t, ref, "alice", "mine")
	_, err = h.Tracker.UpdateComment(ctx, ref, human.ID, core.CommentDraft{Body: "hijacked"})
	wantKind(t, err, core.KindForbidden)
}
