package storage

import (
	"context"
	"database/sql"
	"slices"

	"github.com/ungarscool1/goblins/internal/core"
)

// AppendTranscript implements core.StateStore.
func (s *Store) AppendTranscript(ctx context.Context, entries []core.TranscriptEntry) error {
	if len(entries) == 0 {
		return nil
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, e := range entries {
			if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO transcripts (task_id, job_id, kind, text, at) VALUES (?, ?, ?, ?, ?)`),
				e.TaskID, e.JobID, e.Kind, e.Text, nanos(e.At)); err != nil {
				return err
			}
		}
		return nil
	})
}

// ListTranscript implements core.StateStore.
func (s *Store) ListTranscript(ctx context.Context, taskID, jobID string,
	limit int) ([]core.TranscriptEntry, error) {
	if limit <= 0 {
		limit = 1000
	}
	q, args := `SELECT job_id, kind, text, at FROM transcripts WHERE task_id = ?`, []any{taskID}
	if jobID != "" {
		q, args = q+` AND job_id = ?`, append(args, jobID)
	}
	rows, err := s.db.QueryContext(ctx, s.rebind(q+` ORDER BY id DESC LIMIT ?`),
		append(args, limit)...)
	if err != nil {
		return nil, classify("storage.transcript", err)
	}
	defer func() { _ = rows.Close() }()
	out := []core.TranscriptEntry{}
	for rows.Next() {
		e := core.TranscriptEntry{TaskID: taskID}
		var at int64
		if err := rows.Scan(&e.JobID, &e.Kind, &e.Text, &at); err != nil {
			return nil, classify("storage.transcript", err)
		}
		e.At = fromNanos(at)
		out = append(out, e)
	}
	slices.Reverse(out)
	return out, classify("storage.transcript", rows.Err())
}
