package directory_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
	"github.com/meshsat/meshsat-hub/internal/directory"
	"github.com/meshsat/meshsat-hub/internal/directory/directorytest"
	hubsqlite "github.com/meshsat/meshsat-hub/internal/store/sqlite"

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

// TestRotationFenceSealedSQLite runs the cross-generation rotation cases
// (r20 C7-R7) with the key store production uses: the real SQLite store's
// system_config behind a sealedconfig keeper. The plain-store run is in
// TestConformanceSQLite, the Postgres ones in internal/store/postgres.
func TestRotationFenceSealedSQLite(t *testing.T) {
	directorytest.RotationFence(t, func(t *testing.T) (*directory.SQLStore, directorytest.KeyStore) {
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
		hub, err := hubsqlite.New(filepath.Join(t.TempDir(), "hub.db"), 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = hub.Close() })
		if err := hub.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		wrap, err := hubcrypto.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		return s, directorytest.SealedKeyStore(hub, wrap)
	})
}
