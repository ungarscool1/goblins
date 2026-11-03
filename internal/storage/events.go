package storage

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/ungarscool1/goblins/internal/core"
)

// ListEvents implements core.StateStore.
func (s *Store) ListEvents(ctx context.Context, project string, subject core.Subject,
	limit int) ([]core.Event, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT seq, payload FROM events
		WHERE project = ? AND subject_key = ? ORDER BY seq DESC LIMIT ?`),
		project, subject.Key(), limit)
	if err != nil {
		return nil, classify("storage.list_events", err)
	}
	defer func() { _ = rows.Close() }()
	out := []core.Event{}
	for rows.Next() {
		var seq int64
		var raw string
		if err := rows.Scan(&seq, &raw); err != nil {
			return nil, classify("storage.list_events", err)
		}
		var ev core.Event
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			return nil, core.Wrap(core.KindInternal, "storage.list_events", err)
		}
		ev.Seq = seq
		out = append(out, ev)
	}
	slices.Reverse(out)
	return out, classify("storage.list_events", rows.Err())
}
