package tenancy

import (
	"context"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

func statusStore(t *testing.T) store.Store {
	t.Helper()
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// The cache is what keeps the middleware from being a query per request, so it
// has to answer from cache; Forget is what keeps a suspension from waiting out
// the TTL.
func TestStatusCache(t *testing.T) {
	ctx := context.Background()
	s := statusStore(t)
	if err := s.CreateTenant(ctx, &store.Tenant{ID: "t1", Slug: "t1", Name: "t1", Status: store.TenantActive}); err != nil {
		t.Fatalf("create: %v", err)
	}
	c := NewStatusCache(s, time.Minute)

	if got, err := c.Status(ctx, "t1"); err != nil || got != store.TenantActive {
		t.Fatalf("status = %q, %v; want active", got, err)
	}

	// Change it underneath: the cache must still answer the old value, which
	// is the behaviour Forget exists to correct.
	if err := s.SoftDeleteTenant(ctx, "t1", time.Now()); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if got, _ := c.Status(ctx, "t1"); got != store.TenantActive {
		t.Errorf("status = %q; a warm cache should still say active", got)
	}
	c.Forget("t1")
	if got, _ := c.Status(ctx, "t1"); got != store.TenantDeleted {
		t.Errorf("status after Forget = %q, want %q", got, store.TenantDeleted)
	}
}

// An unknown tenant is not an authorisation failure to invent here, and a
// lookup error must not lock everyone out.
func TestStatusCache_UnknownAndNil(t *testing.T) {
	ctx := context.Background()
	if got, _ := NewStatusCache(statusStore(t), 0).Status(ctx, "nobody"); got != store.TenantActive {
		t.Errorf("unknown tenant = %q, want active", got)
	}
	var nilCache *StatusCache
	if got, err := nilCache.Status(ctx, "x"); err != nil || got != store.TenantActive {
		t.Errorf("nil cache = %q, %v; want active", got, err)
	}
	nilCache.Forget("x") // must not panic
	if err := nilCache.Subscribe(); err != nil {
		t.Errorf("Subscribe on a nil cache: %v", err)
	}
}
