package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		db, err := New(filepath.Join(t.TempDir(), "conf.db"), 0)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if err := db.Migrate(context.Background()); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	})
}
