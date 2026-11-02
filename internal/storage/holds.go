package storage

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// HoldLimit keeps jobs of a limit key from starting. A hold only ever grows, as a job on
// its way out with a shorter wait must not shorten what another one reported.
func (s *Store) HoldLimit(ctx context.Context, h core.LimitHold) error {
	_, err := s.db.ExecContext(ctx, s.rebind(`INSERT INTO limit_holds
		(limit_key, held_until, reason) VALUES (?, ?, ?) ON CONFLICT (limit_key) DO UPDATE SET
		held_until = CASE WHEN excluded.held_until > limit_holds.held_until
			THEN excluded.held_until ELSE limit_holds.held_until END,
		reason = excluded.reason`), h.LimitKey, nanos(h.Until), h.Reason)
	return classify("storage.hold_limit", err)
}

// ReleaseLimit ends a hold, if there is one.
func (s *Store) ReleaseLimit(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM limit_holds WHERE limit_key = ?`), key)
	return classify("storage.release_limit", err)
}

// ActiveHolds lists the holds still in force.
func (s *Store) ActiveHolds(ctx context.Context, now time.Time) ([]core.LimitHold, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT limit_key, held_until, reason
		FROM limit_holds WHERE held_until > ? ORDER BY limit_key`), nanos(now))
	if err != nil {
		return nil, classify("storage.active_holds", err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.LimitHold
	for rows.Next() {
		var h core.LimitHold
		var until int64
		if err := rows.Scan(&h.LimitKey, &until, &h.Reason); err != nil {
			return nil, classify("storage.active_holds", err)
		}
		h.Until = fromNanos(until)
		out = append(out, h)
	}
	return out, classify("storage.active_holds", rows.Err())
}

// claimOrder picks what may start from jobs in queue order. Within a priority, the project
// running the least goes first, so one busy project cannot take every slot from the others.
func claimOrder(candidates []core.Job, running runningCounts, c core.JobClaim) []core.Job {
	var out []core.Job
	for _, j := range candidates {
		if j.Priority < c.MinPriority {
			continue
		}
		if j.LimitKey != "" && (c.Held[j.LimitKey] || !running.allows(j, c)) {
			continue
		}
		out = append(out, j)
	}
	share := func(j core.Job) int {
		if j.LimitKey == "" {
			return 0
		}
		return running.byProject[j.Project]
	}
	slices.SortStableFunc(out, func(a, b core.Job) int {
		return cmp.Or(cmp.Compare(b.Priority, a.Priority), cmp.Compare(share(a), share(b)))
	})
	return out
}
