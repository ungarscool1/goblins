package storage

import (
	"context"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// Stats counts tasks by state and unfinished jobs by kind and state.
func (s *Store) Stats(ctx context.Context) (core.StoreStats, error) {
	stats := core.StoreStats{}
	rows, err := s.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM tasks GROUP BY state`)
	if err != nil {
		return stats, classify("storage.stats", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var c core.StateCount
		if err := rows.Scan(&c.State, &c.Count); err != nil {
			return stats, classify("storage.stats", err)
		}
		stats.Tasks = append(stats.Tasks, c)
	}
	if err := rows.Err(); err != nil {
		return stats, classify("storage.stats", err)
	}
	jobs, err := s.db.QueryContext(ctx, s.rebind(`SELECT kind, state, COUNT(*) FROM jobs
		WHERE state IN (?, ?) GROUP BY kind, state`),
		string(core.JobPending), string(core.JobRunning))
	if err != nil {
		return stats, classify("storage.stats", err)
	}
	defer func() { _ = jobs.Close() }()
	for jobs.Next() {
		var c core.JobCount
		if err := jobs.Scan(&c.Kind, &c.State, &c.Count); err != nil {
			return stats, classify("storage.stats", err)
		}
		stats.Jobs = append(stats.Jobs, c)
	}
	return stats, classify("storage.stats", jobs.Err())
}

// PruneTranscripts removes what finished tasks said before a time, and how much that was.
// A task still open keeps everything, as a person may still need to read it.
func (s *Store) PruneTranscripts(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM transcripts WHERE at < ? AND task_id IN
		(SELECT id FROM tasks WHERE state IN (?, ?, ?))`), nanos(before),
		string(core.StateCompleted), string(core.StateFailed), string(core.StateCancelled))
	if err != nil {
		return 0, classify("storage.prune", err)
	}
	n, err := res.RowsAffected()
	return n, classify("storage.prune", err)
}
