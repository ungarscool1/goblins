package app

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/observability"
)

// WriteAudit says what happened to a task and why, from the durable record, for a person
// working out why it stopped. Reasons and errors were redacted before they were stored.
func WriteAudit(ctx context.Context, w io.Writer, store core.StateStore, id string) error {
	task, err := store.GetTask(ctx, id)
	if err != nil {
		return err
	}
	transitions, err := store.ListTransitions(ctx, id)
	if err != nil {
		return err
	}
	jobs, err := store.ListJobs(ctx, id)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "task %s, project %s: %s\n", task.ID, task.Project, task.State)
	_, _ = fmt.Fprintf(w, "trace %s\n", observability.TraceID(task.ID))
	if task.Reason != "" {
		_, _ = fmt.Fprintf(w, "reason: %s\n", task.Reason)
	}
	if task.Approval != nil {
		_, _ = fmt.Fprintf(w, "design: revision %d, approved by %s\n", task.Approval.Revision,
			task.Approval.Approver.Login)
	}
	if task.HeadSHA != "" {
		_, _ = fmt.Fprintf(w, "commit: %s\n", task.HeadSHA)
	}
	if task.PullRequest != nil {
		_, _ = fmt.Fprintf(w, "pull request: %s\n", task.PullRequest.Number)
	}
	_, _ = fmt.Fprintln(w, "\ntransitions:")
	for _, t := range transitions {
		_, _ = fmt.Fprintf(w, "  %s  %s -> %s  %s\n", t.At.UTC().Format(time.RFC3339), t.From, t.To,
			t.Reason)
	}
	_, _ = fmt.Fprintln(w, "\njobs:")
	for _, j := range jobs {
		_, _ = fmt.Fprintf(w, "  %s  %s  %s  attempt %d", j.CreatedAt.UTC().Format(time.RFC3339),
			j.Kind, j.State, j.Attempt)
		if j.LastError != "" {
			_, _ = fmt.Fprintf(w, "  %s", j.LastError)
		}
		_, _ = fmt.Fprintln(w)
	}
	return nil
}
