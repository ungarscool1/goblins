package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
)

// AcquireLease implements core.LeaseStore in one statement, so two processes racing for a
// resource cannot both win.
func (s *Store) AcquireLease(ctx context.Context, l core.Lease, now time.Time) (core.Lease, error) {
	data, err := json.Marshal(l.Data)
	if err != nil {
		return core.Lease{}, core.Wrap(core.KindInvalid, "storage.acquire_lease", err)
	}
	res, err := s.db.ExecContext(ctx, s.rebind(`INSERT INTO leases
		(resource, holder, acquired_at, expires_at, data) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (resource) DO UPDATE SET
			acquired_at = CASE WHEN leases.holder = excluded.holder
				THEN leases.acquired_at ELSE excluded.acquired_at END,
			data = CASE WHEN leases.holder = excluded.holder
				THEN leases.data ELSE excluded.data END,
			holder = excluded.holder, expires_at = excluded.expires_at
		WHERE leases.holder = excluded.holder OR leases.expires_at <= ?`),
		l.Resource, l.Holder, nanos(now), nanos(l.ExpiresAt), string(data), nanos(now))
	if err != nil {
		return core.Lease{}, classify("storage.acquire_lease", err)
	}
	current, err := s.GetLease(ctx, l.Resource)
	if err != nil {
		return core.Lease{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 || current.Holder != l.Holder {
		return current, core.Errorf(core.KindConflict, "storage.acquire_lease",
			"%s is held by %s until %s", l.Resource, current.Holder,
			current.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return current, nil
}

// GetLease implements core.LeaseStore.
func (s *Store) GetLease(ctx context.Context, resource string) (core.Lease, error) {
	row := s.db.QueryRowContext(ctx, s.rebind(`SELECT resource, holder, acquired_at, expires_at,
		data FROM leases WHERE resource = ?`), resource)
	l, err := scanLease(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Lease{}, core.Errorf(core.KindNotFound, "storage.get_lease", "no lease on %s",
			resource)
	}
	return l, err
}

// UpdateLease implements core.LeaseStore.
func (s *Store) UpdateLease(ctx context.Context, l core.Lease) error {
	data, err := json.Marshal(l.Data)
	if err != nil {
		return core.Wrap(core.KindInvalid, "storage.update_lease", err)
	}
	res, err := s.db.ExecContext(ctx, s.rebind(`UPDATE leases SET expires_at = ?, data = ?
		WHERE resource = ? AND holder = ?`), nanos(l.ExpiresAt), string(data), l.Resource, l.Holder)
	if err != nil {
		return classify("storage.update_lease", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return core.Errorf(core.KindConflict, "storage.update_lease", "%s is not held by %s",
			l.Resource, l.Holder)
	}
	return nil
}

// ReleaseLease implements core.LeaseStore.
func (s *Store) ReleaseLease(ctx context.Context, resource, holder string) error {
	_, err := s.db.ExecContext(ctx,
		s.rebind(`DELETE FROM leases WHERE resource = ? AND holder = ?`),
		resource, holder)
	return classify("storage.release_lease", err)
}

// ListLeases implements core.LeaseStore.
func (s *Store) ListLeases(ctx context.Context, prefix string) ([]core.Lease, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT resource, holder, acquired_at, expires_at,
		data FROM leases WHERE substr(resource, 1, ?) = ? ORDER BY resource`), len(prefix), prefix)
	if err != nil {
		return nil, classify("storage.list_leases", err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.Lease
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, classify("storage.list_leases", rows.Err())
}

func scanLease(row interface{ Scan(...any) error }) (core.Lease, error) {
	var l core.Lease
	var acquired, expires int64
	var data string
	if err := row.Scan(&l.Resource, &l.Holder, &acquired, &expires, &data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return core.Lease{}, err
		}
		return core.Lease{}, classify("storage.lease", err)
	}
	l.AcquiredAt, l.ExpiresAt = fromNanos(acquired), fromNanos(expires)
	if err := json.Unmarshal([]byte(data), &l.Data); err != nil {
		return core.Lease{}, core.Wrap(core.KindInternal, "storage.lease", err)
	}
	return l, nil
}
