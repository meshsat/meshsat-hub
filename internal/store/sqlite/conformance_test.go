package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/storetest"
)

// The conformance suite opens a fresh, empty, migrated store for every one of
// its ~30 sub-tests. Running all 25 migrations each time is the whole cost of
// TestConformance (78 s in CI, MESHSAT-1156): the tests themselves are
// milliseconds. So the migrations run ONCE per test binary into a template
// file, and each sub-test opens its own byte-for-byte copy of that file. The
// copy is opened with exactly the DSN New always uses, so the sub-tests still
// exercise the same journal and synchronous settings as before.
//
// The template is only complete as a single file if nothing is left in a WAL
// sidecar, so it is checkpointed and CLOSED before its bytes are read; the
// sidecar's absence is asserted rather than assumed.
var (
	templateOnce  sync.Once
	templateBytes []byte
	templateErr   error
)

func migratedTemplate(t *testing.T) []byte {
	t.Helper()
	templateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "hub-sqlite-template-")
		if err != nil {
			templateErr = err
			return
		}
		defer func() { _ = os.RemoveAll(dir) }()

		path := filepath.Join(dir, "template.db")
		db, err := New(path, 0)
		if err != nil {
			templateErr = err
			return
		}
		if err := db.Migrate(context.Background()); err != nil {
			_ = db.Close()
			templateErr = err
			return
		}
		// Fold any WAL into the main file before the last connection closes;
		// a no-op under journal_mode=MEMORY (HUB_SQLITE_TEST_FAST=1).
		if _, err := db.rawDB.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			_ = db.Close()
			templateErr = err
			return
		}
		if err := db.Close(); err != nil {
			templateErr = err
			return
		}
		if _, err := os.Stat(path + "-wal"); err == nil {
			templateErr = os.ErrExist
			return
		}
		templateBytes, templateErr = os.ReadFile(path) // #nosec G304 -- path built above from MkdirTemp
	})
	if templateErr != nil {
		t.Fatalf("building the migrated sqlite template: %v", templateErr)
	}
	return templateBytes
}

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		path := filepath.Join(t.TempDir(), "conf.db")
		if err := os.WriteFile(path, migratedTemplate(t), 0o600); err != nil {
			t.Fatalf("copy template: %v", err)
		}
		db, err := New(path, 0)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	})
}
