package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/observability"
)

// observeJob records one finished attempt: as metrics, and as a span tying it to its design
// revision and commit, so a task can be followed from ticket to pull request.
func (s *Scheduler) observeJob(ctx context.Context, log *slog.Logger, task core.Task, job core.Job,
	start time.Time, runErr error) {
	took := s.Now().Sub(start)
	outcome := "ok"
	if runErr != nil {
		outcome = string(core.KindOf(runErr))
	}
	s.Metrics.Inc("goblins_jobs_total",
		observability.Labels{"kind", string(job.Kind), "outcome", outcome})
	s.Metrics.Observe("goblins_job_duration_seconds", took.Seconds(),
		observability.Labels{"kind", string(job.Kind)})
	if job.Attempt > 1 {
		s.Metrics.Inc("goblins_job_retries_total", observability.Labels{"kind", string(job.Kind)})
	}
	attrs := []any{"task", task.ID, "job", job.ID, "kind", job.Kind, "attempt", job.Attempt,
		"design_revision", task.DesignRevision, "head_sha", task.HeadSHA}
	if task.PullRequest != nil {
		attrs = append(attrs, "pull_request", task.PullRequest.Number)
	}
	observability.LogSpan(ctx, log, observability.Span{TaskID: task.ID, Name: "job." + string(job.Kind),
		Start: start, Duration: took, Status: outcome, Attrs: attrs})
}

// checkHealth looks for what nothing else would notice: a task that should be moving, with
// no job to move it. It says so rather than guess at a fix, as such a task means a bug. It also
// applies the retention of transcripts.
func (s *Scheduler) checkHealth(ctx context.Context) {
	s.Stalled.Store(int64(s.countStalled(ctx)))
	if s.Cfg.TranscriptRetention <= 0 {
		return
	}
	n, err := s.Store.PruneTranscripts(ctx, s.Now().Add(-s.Cfg.TranscriptRetention))
	if err != nil {
		s.Log.Warn("cannot prune transcripts", "err", err)
	} else if n > 0 {
		s.Log.Info("pruned old transcripts", "entries", n)
	}
}

func (s *Scheduler) countStalled(ctx context.Context) int {
	tasks, err := s.Store.ListTasks(ctx, "")
	if err != nil {
		s.Log.Warn("cannot list tasks", "err", err)
		return 0
	}
	stalled := 0
	for _, t := range tasks {
		if t.State.IsTerminal() || t.State.IsWaiting() || t.State == core.StateBlocked ||
			t.State == core.StateQuotaPaused || s.Now().Sub(t.UpdatedAt) < s.Cfg.StallAfter {
			continue
		}
		jobs, err := s.Store.ListJobs(ctx, t.ID)
		if err != nil {
			s.Log.Warn("cannot list jobs", "task", t.ID, "err", err)
			continue
		}
		if !hasOpenJob(jobs) {
			stalled++
			s.Log.Warn("task has no job to move it", "task", t.ID, "state", t.State,
				"since", t.UpdatedAt, "trace_id", observability.TraceID(t.ID))
		}
	}
	return stalled
}

func hasOpenJob(jobs []core.Job) bool {
	for _, j := range jobs {
		if j.State == core.JobPending || j.State == core.JobRunning {
			return true
		}
	}
	return false
}

// StalledTasks implements api.StallCounter.
func (s *Scheduler) StalledTasks() int64 { return s.Stalled.Load() }
