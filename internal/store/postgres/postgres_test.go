package postgres

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/rs/xid"
)

// testDSN returns the DSN of a throwaway Postgres or skips. CI provides it via
// the test:postgres job (service postgres:16-alpine); locally:
//
//	docker run -d --rm --name hub-pg -e POSTGRES_HOST_AUTH_METHOD=trust -p 127.0.0.1:55432:5432 postgres:16-alpine
//	HUB_TEST_POSTGRES_DSN=postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable go test ./internal/store/postgres/
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("HUB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("HUB_TEST_POSTGRES_DSN not set")
	}
	return dsn
}

// testDB opens the DSN with a fresh schema so parallel packages/tests do not
// collide, migrates it and drops it on cleanup.
func testDB(t *testing.T) *DB {
	t.Helper()
	dsn := testDSN(t)
	schema := "t_" + xid.New().String()
	admin, err := New(dsn, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	if _, err := admin.rawDB.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	sep := "?"
	if containsRune(dsn, '?') {
		sep = "&"
	}
	db, err := New(dsn+sep+"search_path="+schema, 0)
	if err != nil {
		t.Fatalf("open schema: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_, _ = admin.rawDB.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close()
	})
	return db
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

func TestValidateMigrations(t *testing.T) {
	if err := validateMigrations(); err != nil {
		t.Fatal(err)
	}
	if len(migrations) == 0 || migrations[0].Name != "initial_schema" {
		t.Fatalf("unexpected migration set: %+v", migrations)
	}
}

func TestMigrateIdempotentAndChecksummed(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	v, err := db.AppliedVersion(ctx)
	if err != nil || v != len(migrations) {
		t.Fatalf("applied version = %d (%v), want %d", v, err, len(migrations))
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate must be a no-op: %v", err)
	}
	if err := db.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	// Tamper with the recorded checksum: the runner must refuse to continue.
	if _, err := db.rawDB.ExecContext(ctx, "UPDATE schema_migrations SET checksum = 'tampered' WHERE version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); !errors.Is(err, ErrMigrationChecksum) {
		t.Fatalf("expected ErrMigrationChecksum, got %v", err)
	}
}

func TestSchemaHasEveryTable(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	want := []string{"devices", "messages", "webhook_configs", "delivery_logs", "positions", "audit_log",
		"api_keys", "device_configs", "escalation_chains", "alerts", "notification_prefs", "users",
		"refresh_tokens", "device_keys", "device_wireguard", "routes", "system_config", "bridges",
		"cost_ledger", "device_groups", "device_group_members", "message_templates", "alert_rules",
		"bond_groups", "credentials", "schema_migrations"}
	for _, tbl := range want {
		var n int
		if err := db.rawDB.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = $1", tbl).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s missing (n=%d err=%v)", tbl, n, err)
		}
	}
}
