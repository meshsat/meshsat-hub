package sqlite

import (
	"context"
	"path/filepath"
	"testing"
)

// Migrate runs on EVERY Hub start, against a database that already has the
// schema. So "running it twice is a no-op" is load-bearing, and nothing tested
// it: every existing helper opens a fresh temp file and migrates once.
//
// SQLite has no ALTER TABLE ... ADD COLUMN IF NOT EXISTS, so Migrate swallows
// "duplicate column" errors -- but only for alterMigrations and
// lateAlterMigrations. The plain `migrations` and `postAlterMigrations` slices
// are NOT tolerant.
//
// The dangerous one is postAlterMigrations, and it is dangerous precisely
// because it looks right: `CREATE TABLE tenants` lives there, so putting an
// ALTER for tenants beside it is the natural thing to do. It then works
// perfectly on a fresh database, passes every other test in this package -- all
// of which migrate exactly once -- and fails on the SECOND run with
// "duplicate column name". In production that is a defect that ships green and
// surfaces at the next restart, with the Hub down.
//
// Verified by doing it: moving one of the tenant-settings ALTERs next to the
// CREATE TABLE makes this test fail with
// `Migrate run 2 failed: post-alter migration 6: duplicate column name`.
// (An ALTER in the plain `migrations` slice fails on run 1 instead, because
// tenants does not exist yet -- less dangerous, and also caught here.)
func TestMigrateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	db, err := New(path, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		if err := db.Migrate(ctx); err != nil {
			t.Fatalf("Migrate run %d failed: %v\n"+
				"Every Hub start runs this against an already-migrated database. "+
				"A new ALTER TABLE belongs in alterMigrations, postAlterMigrations or "+
				"lateAlterMigrations, where a duplicate column is tolerated -- not in "+
				"the plain `migrations` slice.", i, err)
		}
	}

	// And the columns the tenant settings live in survived all three runs with
	// their values intact, rather than being re-added and reset.
	if _, err := db.db.ExecContext(ctx,
		`INSERT INTO tenants (id, slug, name) VALUES ('t_restart', 't_restart', 'r')`); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := db.db.ExecContext(ctx,
		`UPDATE tenants SET bridge_offline_timeout=900, audit_retention_days=365,
		 ratelimit_daily_cap=500, ratelimit_monthly_cap=9000 WHERE id='t_restart'`); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate after writing settings: %v", err)
	}
	tn, err := db.GetTenant(ctx, "t_restart")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if tn.BridgeOfflineTimeout != 900 || tn.AuditRetentionDays != 365 ||
		tn.RatelimitDailyCap != 500 || tn.RatelimitMonthlyCap != 9000 {
		t.Errorf("a tenant's settings did not survive a re-run of Migrate: %+v", tn)
	}
}
