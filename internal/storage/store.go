// Package storage implements core.StateStore on SQL databases. One code path
// serves SQLite, for single-node installations, and PostgreSQL, for multi-replica
// ones; only the driver, schema types and placeholder syntax differ.
package storage

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"crypto/rand"
	"encoding/hex"

	// Database drivers: both are pure Go, so the binary stays static.
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/ungarscool1/goblins/internal/core"
)

//go:embed migrations
var migrations embed.FS

// Store is a core.StateStore backed by database/sql.
type Store struct {
	db       *sql.DB
	postgres bool
}

var _ core.StateStore = (*Store)(nil)

// OpenSQLite opens or creates a database file and migrates it.
func OpenSQLite(ctx context.Context, file string) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(1)")
	// Taking the write lock when a transaction starts avoids lock-upgrade
	// failures, which busy_timeout cannot retry.
	q.Add("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+file+"?"+q.Encode())
	if err != nil {
		return nil, core.Wrap(core.KindInternal, "storage.open", err)
	}
	db.SetMaxOpenConns(8)
	return open(ctx, db, false)
}

// OpenPostgres connects to a PostgreSQL DSN and migrates it.
func OpenPostgres(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		// Driver errors can echo the DSN, which holds a password.
		return nil, core.Errorf(core.KindInvalid, "storage.open", "cannot open postgres connection")
	}
	return open(ctx, db, true)
}

func open(ctx context.Context, db *sql.DB, postgres bool) (*Store, error) {
	s := &Store{db: db, postgres: postgres}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

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
		var current int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
			return err
		}
		latest := 0
		for _, name := range names {
			v, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
			if err != nil {
				return fmt.Errorf("bad migration name %s", name)
			}
			latest = max(latest, v)
			if v <= current {
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
		if current > latest {
			return core.Errorf(core.KindInvalid, "storage.migrate", "database schema version %d is newer than this build supports (%d); upgrade Goblins", current, latest)
		}
		return nil
	})
}

// rebind converts ? placeholders for PostgreSQL. No query here has a literal ?.
func (s *Store) rebind(q string) string {
	if !s.postgres {
		return q
	}
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classify("storage.begin", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return classify("storage.tx", err)
	}
	return classify("storage.commit", tx.Commit())
}

// classify keeps kinds already assigned and marks driver errors as transient:
// they are typically contention or connectivity.
func classify(op string, err error) error {
	if err == nil {
		return nil
	}
	var ce *core.Error
	if errors.As(err, &ce) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return core.Wrap(core.KindCancelled, op, err)
	}
	return core.Wrap(core.KindTransient, op, err)
}

func nanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

func newID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}

// Ping implements core.StateStore.
func (s *Store) Ping(ctx context.Context) error {
	return classify("storage.ping", s.db.PingContext(ctx))
}

// Close implements core.StateStore.
func (s *Store) Close() error { return s.db.Close() }

// AppendEvent implements core.StateStore.
func (s *Store) AppendEvent(ctx context.Context, ev core.Event) (core.Event, bool, error) {
	return appendEvent(ctx, s, s.db, ev)
}

type querier interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

func appendEvent(ctx context.Context, s *Store, q querier, ev core.Event) (core.Event, bool, error) {
	if ev.DedupKey == "" || ev.Project == "" {
		return core.Event{}, false, core.Errorf(core.KindInvalid, "storage.append_event", "event needs a dedup key and a project")
	}
	if ev.ReceivedAt.IsZero() {
		ev.ReceivedAt = time.Now()
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return core.Event{}, false, core.Wrap(core.KindInvalid, "storage.append_event", err)
	}
	var seq int64
	err = q.QueryRowContext(ctx, s.rebind(`INSERT INTO events (dedup_key, project, subject_key, payload, received_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (dedup_key) DO NOTHING RETURNING seq`),
		ev.DedupKey, ev.Project, ev.Subject.Key(), string(payload), nanos(ev.ReceivedAt)).Scan(&seq)
	switch {
	case err == nil:
		ev.Seq = seq
		return ev, true, nil
	case errors.Is(err, sql.ErrNoRows):
		var raw string
		if err := q.QueryRowContext(ctx, s.rebind(`SELECT seq, payload FROM events WHERE dedup_key = ?`), ev.DedupKey).Scan(&seq, &raw); err != nil {
			return core.Event{}, false, classify("storage.append_event", err)
		}
		var existing core.Event
		if err := json.Unmarshal([]byte(raw), &existing); err != nil {
			return core.Event{}, false, core.Wrap(core.KindInternal, "storage.append_event", err)
		}
		existing.Seq = seq
		return existing, false, nil
	}
	return core.Event{}, false, classify("storage.append_event", err)
}

// ClaimEvent implements core.StateStore.
func (s *Store) ClaimEvent(ctx context.Context, owner string, leaseFor time.Duration, now time.Time) (*core.Event, error) {
	var out *core.Event
	err := s.tx(ctx, func(tx *sql.Tx) error {
		n := nanos(now)
		// Only the head of a subject's queue qualifies: this serializes processing
		// per subject, and a leased head holds back everything behind it.
		var seq int64
		var raw string
		err := tx.QueryRowContext(ctx, s.rebind(`SELECT e.seq, e.payload FROM events e
			WHERE e.processed_at IS NULL AND e.lease_until <= ? AND e.not_before <= ?
			  AND NOT EXISTS (SELECT 1 FROM events o WHERE o.subject_key = e.subject_key AND o.processed_at IS NULL AND o.seq < e.seq)
			ORDER BY e.seq LIMIT 1`), n, n).Scan(&seq, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, s.rebind(`UPDATE events SET lease_owner = ?, lease_until = ? WHERE seq = ? AND lease_until <= ? AND processed_at IS NULL`),
			owner, nanos(now.Add(leaseFor)), seq, n)
		if err != nil {
			return err
		}
		if c, _ := res.RowsAffected(); c == 0 {
			return nil // lost the race; the caller polls again
		}
		var ev core.Event
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			return core.Wrap(core.KindInternal, "storage.claim_event", err)
		}
		ev.Seq = seq
		out = &ev
		return nil
	})
	return out, err
}

// ReleaseEvent implements core.StateStore.
func (s *Store) ReleaseEvent(ctx context.Context, seq int64, owner string, notBefore time.Time) error {
	_, err := s.db.ExecContext(ctx, s.rebind(`UPDATE events SET lease_owner = '', lease_until = 0, not_before = ? WHERE seq = ? AND lease_owner = ? AND processed_at IS NULL`),
		nanos(notBefore), seq, owner)
	return classify("storage.release_event", err)
}

// Commit implements core.StateStore.
func (s *Store) Commit(ctx context.Context, c core.StateCommit) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.rebind(`UPDATE events SET processed_at = ?, lease_owner = '', lease_until = 0
			WHERE seq = ? AND lease_owner = ? AND processed_at IS NULL`), nanos(c.Now), c.EventSeq, c.EventOwner)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return core.Errorf(core.KindConflict, "storage.commit", "lost the lease on event %d", c.EventSeq)
		}
		if c.Task == nil {
			return nil
		}
		t := *c.Task
		t.UpdatedAt = c.Now
		if c.ExpectedVersion == 0 {
			t.Version = 1
			if t.CreatedAt.IsZero() {
				t.CreatedAt = c.Now
			}
		} else {
			t.Version = c.ExpectedVersion + 1
		}
		data, err := json.Marshal(t)
		if err != nil {
			return core.Wrap(core.KindInternal, "storage.commit", err)
		}
		if c.ExpectedVersion == 0 {
			res, err = tx.ExecContext(ctx, s.rebind(`INSERT INTO tasks (id, project, subject_key, state, version, paused_until, data, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`),
				t.ID, t.Project, t.Subject.Key(), string(t.State), t.Version, nanos(t.PausedUntil), string(data), nanos(t.CreatedAt), nanos(t.UpdatedAt))
		} else {
			res, err = tx.ExecContext(ctx, s.rebind(`UPDATE tasks SET state = ?, version = ?, paused_until = ?, data = ?, updated_at = ? WHERE id = ? AND version = ?`),
				string(t.State), t.Version, nanos(t.PausedUntil), string(data), nanos(t.UpdatedAt), t.ID, c.ExpectedVersion)
		}
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return core.Errorf(core.KindConflict, "storage.commit", "task %s changed concurrently", t.ID)
		}
		if tr := c.Transition; tr != nil {
			if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO transitions (task_id, from_state, to_state, reason, event_seq, at) VALUES (?, ?, ?, ?, ?, ?)`),
				t.ID, string(tr.From), string(tr.To), tr.Reason, c.EventSeq, nanos(c.Now)); err != nil {
				return err
			}
		}
		if d := c.Design; d != nil {
			if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO designs (task_id, revision, hash, body, created_at) VALUES (?, ?, ?, ?, ?)`),
				t.ID, d.Revision, d.Hash, d.Body, nanos(c.Now)); err != nil {
				return err
			}
		}
		for _, k := range c.CancelKinds {
			if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE jobs SET state = ?, lease_owner = '', lease_until = 0, updated_at = ?
				WHERE task_id = ? AND kind = ? AND state IN (?, ?)`),
				string(core.JobCancelled), nanos(c.Now), t.ID, string(k), string(core.JobPending), string(core.JobRunning)); err != nil {
				return err
			}
		}
		for _, j := range c.Jobs {
			if j.IdempotencyKey == "" {
				return core.Errorf(core.KindInvalid, "storage.commit", "job %s has no idempotency key", j.Kind)
			}
			attempts := j.MaxAttempts
			if attempts <= 0 {
				attempts = 3
			}
			if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO jobs (id, task_id, project, kind, idempotency_key, state, max_attempts, priority, limit_key, not_before, payload, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (idempotency_key) DO NOTHING`),
				newID("j-"), t.ID, t.Project, string(j.Kind), j.IdempotencyKey, string(core.JobPending), attempts, j.Priority, j.LimitKey,
				nanos(j.NotBefore), string(j.Payload), nanos(c.Now), nanos(c.Now)); err != nil {
				return err
			}
		}
		return nil
	})
}

func scanTask(row interface{ Scan(...any) error }) (core.Task, error) {
	var raw string
	var version int64
	if err := row.Scan(&raw, &version); err != nil {
		return core.Task{}, err
	}
	var t core.Task
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return core.Task{}, core.Wrap(core.KindInternal, "storage.task", err)
	}
	t.Version = version
	return t, nil
}

// GetTask implements core.StateStore.
func (s *Store) GetTask(ctx context.Context, id string) (core.Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, s.rebind(`SELECT data, version FROM tasks WHERE id = ?`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return core.Task{}, core.Errorf(core.KindNotFound, "storage.get_task", "task %s not found", id)
	}
	return t, classify("storage.get_task", err)
}

// FindTask implements core.StateStore.
func (s *Store) FindTask(ctx context.Context, project string, subject core.Subject) (core.Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, s.rebind(`SELECT data, version FROM tasks WHERE project = ? AND subject_key = ?`), project, subject.Key()))
	if errors.Is(err, sql.ErrNoRows) {
		return core.Task{}, core.Errorf(core.KindNotFound, "storage.find_task", "no task for %s", subject.Key())
	}
	return t, classify("storage.find_task", err)
}

func (s *Store) tasks(ctx context.Context, q string, args ...any) ([]core.Task, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(q), args...)
	if err != nil {
		return nil, classify("storage.tasks", err)
	}
	defer func() { _ = rows.Close() }()
	out := []core.Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, classify("storage.tasks", err)
		}
		out = append(out, t)
	}
	return out, classify("storage.tasks", rows.Err())
}

// ListTasks implements core.StateStore.
func (s *Store) ListTasks(ctx context.Context, project string) ([]core.Task, error) {
	if project == "" {
		return s.tasks(ctx, `SELECT data, version FROM tasks ORDER BY created_at`)
	}
	return s.tasks(ctx, `SELECT data, version FROM tasks WHERE project = ? ORDER BY created_at`, project)
}

// DueTasks implements core.StateStore.
func (s *Store) DueTasks(ctx context.Context, now time.Time) ([]core.Task, error) {
	return s.tasks(ctx, `SELECT data, version FROM tasks WHERE paused_until > 0 AND paused_until <= ? ORDER BY paused_until`, nanos(now))
}

// ListTransitions implements core.StateStore.
func (s *Store) ListTransitions(ctx context.Context, taskID string) ([]core.Transition, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT from_state, to_state, reason, event_seq, at FROM transitions WHERE task_id = ? ORDER BY id`), taskID)
	if err != nil {
		return nil, classify("storage.transitions", err)
	}
	defer func() { _ = rows.Close() }()
	out := []core.Transition{}
	for rows.Next() {
		t := core.Transition{TaskID: taskID}
		var from, to string
		var at int64
		if err := rows.Scan(&from, &to, &t.Reason, &t.EventSeq, &at); err != nil {
			return nil, classify("storage.transitions", err)
		}
		t.From, t.To, t.At = core.State(from), core.State(to), fromNanos(at)
		out = append(out, t)
	}
	return out, classify("storage.transitions", rows.Err())
}

// GetDesign implements core.StateStore.
func (s *Store) GetDesign(ctx context.Context, taskID string, revision int) (core.Design, error) {
	d := core.Design{TaskID: taskID, Revision: revision}
	var at int64
	err := s.db.QueryRowContext(ctx, s.rebind(`SELECT hash, body, created_at FROM designs WHERE task_id = ? AND revision = ?`), taskID, revision).Scan(&d.Hash, &d.Body, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Design{}, core.Errorf(core.KindNotFound, "storage.get_design", "design revision %d not found", revision)
	}
	d.CreatedAt = fromNanos(at)
	return d, classify("storage.get_design", err)
}

const jobColumns = `id, task_id, project, kind, idempotency_key, state, attempt, max_attempts, priority, limit_key, not_before, lease_owner, lease_until, payload, checkpoint, last_error, created_at, updated_at`

func scanJob(row interface{ Scan(...any) error }) (core.Job, error) {
	var j core.Job
	var kind, state, payload, checkpoint string
	var notBefore, leaseUntil, created, updated int64
	if err := row.Scan(&j.ID, &j.TaskID, &j.Project, &kind, &j.IdempotencyKey, &state, &j.Attempt, &j.MaxAttempts, &j.Priority, &j.LimitKey,
		&notBefore, &j.LeaseOwner, &leaseUntil, &payload, &checkpoint, &j.LastError, &created, &updated); err != nil {
		return core.Job{}, err
	}
	j.Kind, j.State = core.JobKind(kind), core.JobState(state)
	j.NotBefore, j.LeaseUntil, j.CreatedAt, j.UpdatedAt = fromNanos(notBefore), fromNanos(leaseUntil), fromNanos(created), fromNanos(updated)
	if payload != "" {
		j.Payload = json.RawMessage(payload)
	}
	if checkpoint != "" {
		j.Checkpoint = json.RawMessage(checkpoint)
	}
	return j, nil
}

func collectJobs(rows *sql.Rows) ([]core.Job, error) {
	defer func() { _ = rows.Close() }()
	out := []core.Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ClaimJob implements core.StateStore.
func (s *Store) ClaimJob(ctx context.Context, c core.JobClaim) (*core.Job, error) {
	var out *core.Job
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if s.postgres {
			// Limit counting and claiming must be one critical section across replicas.
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(726355)`); err != nil {
				return err
			}
		}
		now := nanos(c.Now)
		if err := s.failExhausted(ctx, tx, c.Now); err != nil {
			return err
		}
		running, err := heavyRunning(ctx, s, tx, now)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, s.rebind(`SELECT `+jobColumns+` FROM jobs
			WHERE (state = ? AND not_before <= ?) OR (state = ? AND lease_until <= ?)
			ORDER BY priority DESC, created_at, id`), string(core.JobPending), now, string(core.JobRunning), now)
		if err != nil {
			return err
		}
		candidates, err := collectJobs(rows)
		if err != nil {
			return err
		}
		for _, j := range candidates {
			if j.LimitKey != "" && !running.allows(j, c) {
				continue
			}
			j.Attempt++
			j.State = core.JobRunning
			j.LeaseOwner = c.Owner
			j.LeaseUntil = c.Now.Add(c.LeaseFor)
			if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE jobs SET state = ?, attempt = ?, lease_owner = ?, lease_until = ?, updated_at = ? WHERE id = ?`),
				string(j.State), j.Attempt, j.LeaseOwner, nanos(j.LeaseUntil), now, j.ID); err != nil {
				return err
			}
			out = &j
			return nil
		}
		return nil
	})
	return out, err
}

type runningCounts struct {
	global    int
	byProject map[string]int
	byLimit   map[string]int
}

func (r runningCounts) allows(j core.Job, c core.JobClaim) bool {
	if c.MaxGlobal > 0 && r.global >= c.MaxGlobal {
		return false
	}
	if c.MaxPerProject > 0 && r.byProject[j.Project] >= c.MaxPerProject {
		return false
	}
	if m, ok := c.MaxPerLimit[j.LimitKey]; ok && m > 0 && r.byLimit[j.LimitKey] >= m {
		return false
	}
	return true
}

func heavyRunning(ctx context.Context, s *Store, tx *sql.Tx, now int64) (runningCounts, error) {
	rc := runningCounts{byProject: map[string]int{}, byLimit: map[string]int{}}
	rows, err := tx.QueryContext(ctx, s.rebind(`SELECT project, limit_key FROM jobs WHERE state = ? AND lease_until > ? AND limit_key <> ''`), string(core.JobRunning), now)
	if err != nil {
		return rc, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p, k string
		if err := rows.Scan(&p, &k); err != nil {
			return rc, err
		}
		rc.global++
		rc.byProject[p]++
		rc.byLimit[k]++
	}
	return rc, rows.Err()
}

// failExhausted ends jobs whose workers kept dying, with an outcome the workflow
// can react to. Without it a poisonous job would be retried forever.
func (s *Store) failExhausted(ctx context.Context, tx *sql.Tx, now time.Time) error {
	rows, err := tx.QueryContext(ctx, s.rebind(`SELECT `+jobColumns+` FROM jobs WHERE state = ? AND lease_until <= ? AND attempt >= max_attempts`),
		string(core.JobRunning), nanos(now))
	if err != nil {
		return err
	}
	jobs, err := collectJobs(rows)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		t, err := scanTask(tx.QueryRowContext(ctx, s.rebind(`SELECT data, version FROM tasks WHERE id = ?`), j.TaskID))
		if err != nil {
			return err
		}
		const msg = "the worker was lost repeatedly while running this job"
		if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE jobs SET state = ?, lease_owner = '', lease_until = 0, last_error = ?, updated_at = ? WHERE id = ?`),
			string(core.JobFailed), msg, nanos(now), j.ID); err != nil {
			return err
		}
		ev := core.Event{
			Project: j.Project, Source: "engine", Type: core.EventJobFinished, Subject: t.Subject,
			DedupKey: "job:" + j.ID + ":exhausted", OccurredAt: now, ReceivedAt: now,
			Job: &core.JobOutcome{JobID: j.ID, Kind: j.Kind, Key: j.IdempotencyKey, ErrorKind: core.KindTimeout, Message: msg},
		}
		if _, _, err := appendEvent(ctx, s, tx, ev); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) leaseUpdate(ctx context.Context, op, q string, args ...any) error {
	res, err := s.db.ExecContext(ctx, s.rebind(q), args...)
	if err != nil {
		return classify(op, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return core.Errorf(core.KindConflict, op, "job lease lost")
	}
	return nil
}

// HeartbeatJob implements core.StateStore.
func (s *Store) HeartbeatJob(ctx context.Context, jobID, owner string, attempt int, until time.Time) error {
	return s.leaseUpdate(ctx, "storage.heartbeat", `UPDATE jobs SET lease_until = ? WHERE id = ? AND lease_owner = ? AND attempt = ? AND state = ?`,
		nanos(until), jobID, owner, attempt, string(core.JobRunning))
}

// SaveCheckpoint implements core.StateStore.
func (s *Store) SaveCheckpoint(ctx context.Context, jobID, owner string, attempt int, data json.RawMessage) error {
	return s.leaseUpdate(ctx, "storage.checkpoint", `UPDATE jobs SET checkpoint = ? WHERE id = ? AND lease_owner = ? AND attempt = ? AND state = ?`,
		string(data), jobID, owner, attempt, string(core.JobRunning))
}

// RetryJob implements core.StateStore.
func (s *Store) RetryJob(ctx context.Context, jobID, owner string, attempt int, notBefore time.Time, lastError string) error {
	return s.leaseUpdate(ctx, "storage.retry", `UPDATE jobs SET state = ?, lease_owner = '', lease_until = 0, not_before = ?, last_error = ? WHERE id = ? AND lease_owner = ? AND attempt = ? AND state = ?`,
		string(core.JobPending), nanos(notBefore), lastError, jobID, owner, attempt, string(core.JobRunning))
}

// FinishJob implements core.StateStore.
func (s *Store) FinishJob(ctx context.Context, f core.JobFinish) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.rebind(`UPDATE jobs SET state = ?, last_error = ?, lease_owner = '', lease_until = 0, updated_at = ?
			WHERE id = ? AND lease_owner = ? AND attempt = ? AND state = ?`),
			string(f.State), f.Error, nanos(f.Now), f.JobID, f.Owner, f.Attempt, string(core.JobRunning))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return core.Errorf(core.KindConflict, "storage.finish_job", "job lease lost")
		}
		if f.Event != nil {
			if _, _, err := appendEvent(ctx, s, tx, *f.Event); err != nil {
				return err
			}
		}
		return nil
	})
}

// GetJob implements core.StateStore.
func (s *Store) GetJob(ctx context.Context, id string) (core.Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx, s.rebind(`SELECT `+jobColumns+` FROM jobs WHERE id = ?`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return core.Job{}, core.Errorf(core.KindNotFound, "storage.get_job", "job %s not found", id)
	}
	return j, classify("storage.get_job", err)
}

// ListJobs implements core.StateStore.
func (s *Store) ListJobs(ctx context.Context, taskID string) ([]core.Job, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT `+jobColumns+` FROM jobs WHERE task_id = ? ORDER BY created_at, id`), taskID)
	if err != nil {
		return nil, classify("storage.list_jobs", err)
	}
	jobs, err := collectJobs(rows)
	return jobs, classify("storage.list_jobs", err)
}

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
func (s *Store) ListTranscript(ctx context.Context, taskID string, limit int) ([]core.TranscriptEntry, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT job_id, kind, text, at FROM transcripts WHERE task_id = ? ORDER BY id DESC LIMIT ?`), taskID, limit)
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

// GetCursor implements core.StateStore. A missing cursor is the empty string.
func (s *Store) GetCursor(ctx context.Context, name string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, s.rebind(`SELECT value FROM cursors WHERE name = ?`), name).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, classify("storage.get_cursor", err)
}

// SetCursor implements core.StateStore.
func (s *Store) SetCursor(ctx context.Context, name, value string) error {
	_, err := s.db.ExecContext(ctx, s.rebind(`INSERT INTO cursors (name, value) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET value = excluded.value`), name, value)
	return classify("storage.set_cursor", err)
}
