package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/ungarscool1/goblins/internal/core"
)

// PutMemory implements core.MemoryStore. The record is kept whole, as JSON: its shape can grow
// without a migration, and searching happens in Go, deterministically.
func (s *Store) PutMemory(ctx context.Context, m core.MemoryRecord) error {
	if m.ID == "" || m.Namespace == "" {
		return core.Errorf(core.KindInvalid, "storage.put_memory",
			"a memory needs an ID and a namespace")
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return core.Wrap(core.KindInvalid, "storage.put_memory", err)
	}
	_, err = s.db.ExecContext(ctx,
		s.rebind(`INSERT INTO memories (id, namespace, status, updated_at,
		record) VALUES (?, ?, ?, ?,
			?) ON CONFLICT (id) DO UPDATE SET namespace = excluded.namespace,
		status = excluded.status, updated_at = excluded.updated_at, record = excluded.record`),
		m.ID, m.Namespace, string(m.Status), nanos(m.UpdatedAt), string(raw))
	return classify("storage.put_memory", err)
}

// GetMemory implements core.MemoryStore.
func (s *Store) GetMemory(ctx context.Context, id string) (core.MemoryRecord, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, s.rebind(`SELECT record FROM memories WHERE id = ?`), id).
		Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return core.MemoryRecord{}, core.Errorf(core.KindNotFound, "storage.get_memory",
			"no memory %s", id)
	}
	if err != nil {
		return core.MemoryRecord{}, classify("storage.get_memory", err)
	}
	var m core.MemoryRecord
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return core.MemoryRecord{}, core.Wrap(core.KindInternal, "storage.get_memory", err)
	}
	return m, nil
}

// ListMemories implements core.MemoryStore.
func (s *Store) ListMemories(ctx context.Context, namespaces []string) ([]core.MemoryRecord,
	error) {
	if len(namespaces) == 0 {
		return nil, nil
	}
	args := make([]any, len(namespaces))
	for i, n := range namespaces {
		args[i] = n
	}
	q := `SELECT record FROM memories WHERE namespace IN (?` +
		strings.Repeat(", ?", len(namespaces)-1) + `) ORDER BY updated_at DESC, id`
	rows, err := s.db.QueryContext(ctx, s.rebind(q), args...)
	if err != nil {
		return nil, classify("storage.list_memories", err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.MemoryRecord
	for rows.Next() {
		var raw string
		var m core.MemoryRecord
		if err := rows.Scan(&raw); err != nil {
			return nil, classify("storage.list_memories", err)
		}
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, core.Wrap(core.KindInternal, "storage.list_memories", err)
		}
		out = append(out, m)
	}
	return out, classify("storage.list_memories", rows.Err())
}

// DeleteMemory implements core.MemoryStore. Forgetting is permanent.
func (s *Store) DeleteMemory(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM memories WHERE id = ?`), id)
	return classify("storage.delete_memory", err)
}
