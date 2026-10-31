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
