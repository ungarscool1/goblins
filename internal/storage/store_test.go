package storage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ungarscool1/goblins/internal/core"
	"github.com/ungarscool1/goblins/internal/storage/storetest"
)

func TestSQLiteStore(t *testing.T) {
	storetest.Run(t, func(t *testing.T) core.StateStore {
		s, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "goblins.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

// TestPostgresStore needs GOBLINS_TEST_POSTGRES_DSN, pointing at a disposable database.
func TestPostgresStore(t *testing.T) {
	dsn := os.Getenv("GOBLINS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("GOBLINS_TEST_POSTGRES_DSN not set")
	}
	n := 0
	storetest.Run(t, func(t *testing.T) core.StateStore {
		ctx := context.Background()
		n++
		admin, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = admin.Close() }()
		schema := "t" + strconv.FormatInt(time.Now().UnixNano(), 36) + strconv.Itoa(n)
		if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			a, _ := sql.Open("pgx", dsn)
			defer func() { _ = a.Close() }()
			_, _ = a.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE")
		})
		sep := "?"
		for _, c := range dsn {
			if c == '?' {
				sep = "&"
			}
		}
		s, err := OpenPostgres(ctx, dsn+sep+"search_path="+schema)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestMigrationsPersistAndRefuseNewerSchema(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "goblins.db")
	s, err := OpenSQLite(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetCursor(ctx, "c", "kept"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	s, err = OpenSQLite(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := s.GetCursor(ctx, "c"); v != "kept" {
		t.Fatalf("data lost across reopen: %q", v)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES (9999)`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	if _, err := OpenSQLite(ctx, file); !core.IsKind(err, core.KindInvalid) {
		t.Fatalf("newer schema accepted: %v", err)
	}
}

func TestRebind(t *testing.T) {
	s := &Store{postgres: true}
	if got := s.rebind(`a = ? AND b = ?`); got != `a = $1 AND b = $2` {
		t.Fatal(got)
	}
	if got := (&Store{}).rebind(`a = ?`); got != `a = ?` {
		t.Fatal(got)
	}
}

func TestFindTaskByPullRequest(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	now := time.Unix(1000, 0).UTC()
	add := func(id string, state core.State, number string) {
		t.Helper()
		seq := appendForTest(t, s, id)
		task := core.Task{ID: id, Project: "p", Kind: core.TaskIssue, State: state, CreatedAt: now,
			Subject:     core.Subject{Kind: core.SubjectIssue, Container: "o/r", ExternalID: id},
			PullRequest: &core.PullRequest{Repo: core.RepoRef{Slug: "o/r"}, Number: number}}
		if err := s.Commit(ctx, core.StateCommit{EventSeq: seq, EventOwner: "o", Task: &task, Now: now}); err != nil {
			t.Fatal(err)
		}
	}
	add("a", core.StateAwaitingReview, "7")
	add("b", core.StateCompleted, "8")
	got, err := s.FindTaskByPullRequest(ctx, "p", "o/r", "7")
	if err != nil || got.ID != "a" {
		t.Fatalf("task = %+v, %v", got, err)
	}
	for _, tc := range [][3]string{{"p", "o/r", "8"}, {"p", "o/r", "9"}, {"p", "o/other", "7"}, {"q", "o/r", "7"}} {
		if _, err := s.FindTaskByPullRequest(ctx, tc[0], tc[1], tc[2]); !core.IsKind(err, core.KindNotFound) {
			t.Errorf("%v: %v", tc, err)
		}
	}
}

func appendForTest(t *testing.T, s *Store, key string) int64 {
	t.Helper()
	ctx := context.Background()
	now := time.Unix(1000, 0).UTC()
	if _, _, err := s.AppendEvent(ctx, core.Event{Project: "p", Type: core.EventIssueOpened, DedupKey: key, Subject: core.Subject{ExternalID: key}, ReceivedAt: now}); err != nil {
		t.Fatal(err)
	}
	ev, err := s.ClaimEvent(ctx, "o", time.Minute, now)
	if err != nil || ev == nil {
		t.Fatalf("claim: %+v, %v", ev, err)
	}
	return ev.Seq
}
