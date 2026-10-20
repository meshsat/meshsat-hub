package directory_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/directory"
	"github.com/meshsat/meshsat-hub/internal/directory/directorytest"

	_ "modernc.org/sqlite"
)

// sqliteDSN is the form main.go opens the directory's SQLite handle with.
const sqliteDSN = "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"

// TestConformanceSQLite runs the directory contract on SQLite; the
// Postgres run is in internal/store/postgres.
func TestConformanceSQLite(t *testing.T) {
	directorytest.Run(t, func(t *testing.T) *directory.SQLStore {
		db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "dir.db")+sqliteDSN)
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
		s := directory.NewSQLStore(db)
		if err := s.Migrate(context.Background()); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		return s
	})
}
