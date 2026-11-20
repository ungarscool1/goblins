package storage

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/ungarscool1/goblins/internal/core"
)

func (s *Store) migrate(ctx context.Context) error {
	dir := "migrations/sqlite"
	if s.postgres {
		dir = "migrations/postgres"
	}
	entries, err := fs.ReadDir(migrations, dir)
	if err != nil {
		return core.Wrap(core.KindInternal, "storage.migrate", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)

	return s.tx(ctx, func(tx *sql.Tx) error {
		if s.postgres {
			// Replicas starting together must not migrate concurrently.
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(726354)`); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
			return err
		}
		// Applied as a set, not up to a maximum: migrations from changes merged in any order
		// all run, each once.
		applied, err := appliedVersions(ctx, tx)
		if err != nil {
			return err
		}
		known := map[int]bool{}
		for _, name := range names {
			v, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
			if err != nil {
				return fmt.Errorf("bad migration name %s", name)
			}
			known[v] = true
			if applied[v] {
				continue
			}
			body, err := migrations.ReadFile(path.Join(dir, name))
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return fmt.Errorf("migration %s: %w", name, err)
			}
			if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO schema_migrations (version) VALUES (?)`), v); err != nil {
				return err
			}
		}
		current, latest := 0, 0
		for v := range applied {
			if !known[v] {
				current = max(current, v)
			}
		}
		for v := range known {
			latest = max(latest, v)
		}
		if current > 0 {
			return core.Errorf(core.KindInvalid, "storage.migrate", "database schema version %d is newer than this build supports (%d); upgrade Goblins", current, latest)
		}
		return nil
	})
}

// appliedVersions reads which migrations a database already has.
func appliedVersions(ctx context.Context, tx *sql.Tx) (map[int]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}
