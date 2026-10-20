package directory

import (
	"context"
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "dir.db")+
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSQLiteMigrateIsIdempotent(t *testing.T) {
	s := NewSQLStore(openSQLite(t))
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate %d: %v", i+1, err)
		}
	}
	if v1, err := s.sqliteHasV1Schema(ctx); err != nil || v1 {
		t.Fatalf("fresh schema read as v1: %v %v", v1, err)
	}
}

func TestPostgresMigrateLeavesTheSchemaToTheStore(t *testing.T) {
	// A Postgres store never runs DDL itself: the store migrations own the
	// schema (migration 40) and run it under their advisory lock. A nil
	// handle proves no statement is issued.
	if err := NewPostgresStore(nil).Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// v1Schema is the first directory schema (until MESHSAT-1584 C7): ids and
// addresses unique across every tenant, no tenant on the child tables.
var v1Schema = []string{
	`CREATE TABLE directory_contacts (id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL DEFAULT '', display_name TEXT NOT NULL,
		given_name TEXT NOT NULL DEFAULT '', family_name TEXT NOT NULL DEFAULT '', org TEXT NOT NULL DEFAULT '',
		role TEXT NOT NULL DEFAULT '', team TEXT NOT NULL DEFAULT '', sidc TEXT NOT NULL DEFAULT '', notes TEXT NOT NULL DEFAULT '',
		trust_level INTEGER NOT NULL DEFAULT 0, trust_verified_at TEXT, origin TEXT NOT NULL DEFAULT 'hub',
		policy_id TEXT NOT NULL DEFAULT '', hub_version INTEGER NOT NULL DEFAULT 0, hub_etag TEXT NOT NULL DEFAULT '',
		metadata TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at TEXT NOT NULL DEFAULT (datetime('now')))`,
	`CREATE INDEX idx_dir_contacts_tenant ON directory_contacts(tenant_id)`,
	`CREATE INDEX idx_dir_contacts_name ON directory_contacts(display_name)`,
	`CREATE TABLE directory_addresses (id TEXT PRIMARY KEY,
		contact_id TEXT NOT NULL REFERENCES directory_contacts(id) ON DELETE CASCADE, kind TEXT NOT NULL, value TEXT NOT NULL,
		subvalue TEXT NOT NULL DEFAULT '', label TEXT NOT NULL DEFAULT '', primary_rank INTEGER NOT NULL DEFAULT 0,
		verified INTEGER NOT NULL DEFAULT 0, bearer_hint INTEGER NOT NULL DEFAULT 50, max_cost_cents INTEGER,
		created_at TEXT NOT NULL DEFAULT (datetime('now')), updated_at TEXT NOT NULL DEFAULT (datetime('now')),
		UNIQUE(kind, value))`,
	`CREATE INDEX idx_dir_addr_contact ON directory_addresses(contact_id)`,
	`CREATE TABLE directory_contact_keys (id TEXT PRIMARY KEY,
		contact_id TEXT NOT NULL REFERENCES directory_contacts(id) ON DELETE CASCADE, kind TEXT NOT NULL,
		version INTEGER NOT NULL DEFAULT 1, status TEXT NOT NULL DEFAULT 'active', public_data BLOB,
		fingerprint TEXT NOT NULL DEFAULT '', algorithm TEXT NOT NULL DEFAULT '', trust_anchor TEXT NOT NULL DEFAULT 'hub',
		created_at TEXT NOT NULL DEFAULT (datetime('now')), UNIQUE(contact_id, kind, version))`,
	`CREATE TABLE directory_groups (id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL DEFAULT '', name TEXT NOT NULL,
		kind TEXT NOT NULL DEFAULT 'LIST', sidc TEXT NOT NULL DEFAULT '', mls_group_id TEXT NOT NULL DEFAULT '',
		policy_id TEXT NOT NULL DEFAULT '', hub_version INTEGER NOT NULL DEFAULT 0, hub_etag TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (datetime('now')), updated_at TEXT NOT NULL DEFAULT (datetime('now')))`,
	`CREATE TABLE directory_group_members (group_id TEXT NOT NULL REFERENCES directory_groups(id) ON DELETE CASCADE,
		contact_id TEXT NOT NULL REFERENCES directory_contacts(id) ON DELETE CASCADE, role TEXT NOT NULL DEFAULT '',
		added_at TEXT NOT NULL DEFAULT (datetime('now')), PRIMARY KEY (group_id, contact_id))`,
	`CREATE INDEX idx_dir_group_members_contact ON directory_group_members(contact_id)`,
	`CREATE TABLE directory_dispatch_policy (id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL DEFAULT '',
		name TEXT NOT NULL DEFAULT '', scope_type TEXT NOT NULL DEFAULT 'contact', scope_id TEXT NOT NULL DEFAULT '',
		strategy TEXT NOT NULL DEFAULT 'PRIMARY_ONLY', preferred TEXT NOT NULL DEFAULT '[]', fallback TEXT NOT NULL DEFAULT '[]',
		require_encryption INTEGER NOT NULL DEFAULT 0, max_retries INTEGER NOT NULL DEFAULT 0,
		retry_delay_ns INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at TEXT NOT NULL DEFAULT (datetime('now')))`,
	`CREATE TABLE directory_versions (tenant_id TEXT PRIMARY KEY, version INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL DEFAULT (datetime('now')))`,
}

// TestSQLiteUpgradeFromV1 builds the first schema with rows in it, runs
// Migrate, and checks every row moved with its tenant, the two things the
// old schema allowed and the new one does not were dropped, and the store
// works on the result.
func TestSQLiteUpgradeFromV1(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()
	for _, stmt := range v1Schema {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("v1 schema: %v", err)
		}
	}
	fixture := []string{
		`INSERT INTO directory_contacts (id, tenant_id, display_name, team, trust_verified_at, metadata, created_at, updated_at)
			VALUES ('ca', 'A', 'Alice', 'Red', '2026-01-02 03:04:05', '{"k":"v"}', '2026-01-01 00:00:00', '2026-01-02 00:00:00')`,
		`INSERT INTO directory_contacts (id, tenant_id, display_name) VALUES ('cb', 'B', 'Bob')`,
		`INSERT INTO directory_addresses (id, contact_id, kind, value, primary_rank, verified, max_cost_cents)
			VALUES ('aa', 'ca', 'SMS', '+31611111111', 0, 1, 7)`,
		`INSERT INTO directory_addresses (id, contact_id, kind, value) VALUES ('ab', 'cb', 'SMS', '+31622222222')`,
		`INSERT INTO directory_contact_keys (id, contact_id, kind, public_data) VALUES ('ka', 'ca', 'X25519_LT_PUB', x'0102')`,
		`INSERT INTO directory_groups (id, tenant_id, name, kind) VALUES ('ga', 'A', 'Team A', 'TEAM')`,
		`INSERT INTO directory_group_members (group_id, contact_id, added_at) VALUES ('ga', 'ca', '2026-01-03 00:00:00')`,
		`INSERT INTO directory_group_members (group_id, contact_id, added_at) VALUES ('ga', 'cb', '2026-01-03 00:00:00')`,
		`INSERT INTO directory_dispatch_policy (id, tenant_id, scope_type, scope_id, strategy, created_at)
			VALUES ('p-seed', 'A', 'precedence', 'Flash', 'HEMB_BONDED', '2026-01-01 00:00:00')`,
		`INSERT INTO directory_dispatch_policy (id, tenant_id, scope_type, scope_id, strategy, created_at)
			VALUES ('p-dup', 'A', 'precedence', 'Flash', 'PRIMARY_ONLY', '2026-01-05 00:00:00')`,
		`INSERT INTO directory_dispatch_policy (id, tenant_id, name, scope_type, scope_id, strategy)
			VALUES ('p-op', 'A', 'mine', 'precedence', 'Routine', 'PRIMARY_ONLY')`,
		`INSERT INTO directory_versions (tenant_id, version) VALUES ('A', 5)`,
	}
	for _, stmt := range fixture {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("v1 fixture: %v", err)
		}
	}

	s := NewSQLStore(db)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if v1, _ := s.sqliteHasV1Schema(ctx); v1 {
		t.Fatal("still v1 after the upgrade")
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate after the upgrade: %v", err)
	}

	alice, err := s.GetContact(ctx, "A", "ca")
	if err != nil || alice == nil {
		t.Fatalf("alice: %+v %v", alice, err)
	}
	if alice.Team != "Red" || alice.Metadata["k"] != "v" || alice.TrustVerifiedAt == nil ||
		!alice.CreatedAt.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("alice fields: %+v", alice)
	}
	if len(alice.Addresses) != 1 || !alice.Addresses[0].Verified || alice.Addresses[0].MaxCostCents == nil ||
		*alice.Addresses[0].MaxCostCents != 7 {
		t.Errorf("alice addresses: %+v", alice.Addresses)
	}
	if len(alice.Keys) != 1 || len(alice.Keys[0].Public) != 2 {
		t.Errorf("alice keys: %+v", alice.Keys)
	}
	if bob, _ := s.GetContact(ctx, "B", "cb"); bob == nil || len(bob.Addresses) != 1 || len(bob.GroupIDs) != 0 {
		t.Errorf("bob: %+v", bob)
	}
	g, _ := s.GetGroup(ctx, "A", "ga")
	if g == nil || len(g.MemberIDs) != 1 || g.MemberIDs[0] != "ca" {
		t.Errorf("group A after the upgrade: %+v (the cross-tenant member must be gone)", g)
	}
	pols, _ := s.ListPolicies(ctx, "A")
	byID := map[string]DispatchPolicy{}
	for _, p := range pols {
		byID[p.ID] = p
	}
	if _, ok := byID["p-dup"]; ok || len(pols) != 2 {
		t.Errorf("policies after the upgrade: %+v (the later duplicate scope must be gone)", pols)
	}
	if !byID["p-seed"].Seeded || byID["p-op"].Seeded {
		t.Errorf("seeded marks: seed=%v op=%v", byID["p-seed"].Seeded, byID["p-op"].Seeded)
	}
	if v, _ := s.CurrentVersion(ctx, "A"); v != 5 {
		t.Errorf("version A after the upgrade: %d, want 5", v)
	}
	// What the old schema refused across tenants, the new one allows.
	if err := s.PutContact(ctx, &Contact{TenantID: "B", DisplayName: "Bob 2",
		Addresses: []Address{{Kind: KindSMS, Value: "+31611111111"}}}); err != nil {
		t.Errorf("tenant B taking an address tenant A holds: %v", err)
	}
	if v, _ := s.CurrentVersion(ctx, "A"); v != 5 {
		t.Errorf("tenant B's put moved A's version to %d", v)
	}
	var idx int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name IN
		('idx_dir_contacts_name', 'idx_dir_addr_contact', 'idx_dir_members_contact')`).Scan(&idx); err != nil || idx != 3 {
		t.Errorf("indexes after the upgrade: %d %v", idx, err)
	}
}

func TestDBTimeScansEitherDriver(t *testing.T) {
	want := time.Date(2026, 10, 20, 15, 4, 5, 0, time.UTC)
	for _, v := range []any{
		"2026-10-20 15:04:05", []byte("2026-10-20 15:04:05"), "2026-10-20T15:04:05Z",
		"2026-10-20 17:04:05+02:00", want, want.In(time.FixedZone("CEST", 7200)),
	} {
		var d dbTime
		if err := d.Scan(v); err != nil || !d.valid || !d.t.Equal(want) || d.t.Location() != time.UTC {
			t.Errorf("%T %v: %v valid=%v err=%v", v, v, d.t, d.valid, err)
		}
	}
	var d dbTime
	if err := d.Scan(nil); err != nil || d.valid {
		t.Errorf("NULL: valid=%v err=%v", d.valid, err)
	}
	if err := d.Scan("yesterday"); err == nil {
		t.Error("garbage time accepted")
	}
}

var (
	reDirTable = regexp.MustCompile(`(?i)\b(?:FROM|INTO|UPDATE|JOIN)\s+directory_[a-z0-9_]+`)
	reDDL      = regexp.MustCompile(`(?i)^\s*(?:CREATE|ALTER|DROP)\b`)
	reTenant   = regexp.MustCompile(`\btenant_id\b`)
)

// TestEveryDirectoryStatementNamesTheTenant is this package's tenant
// ratchet (the store packages have theirs in internal/store/scoping_test.go,
// which does not read this package): every statement that reads or writes a
// directory table names tenant_id, with no exceptions. The first schema
// keyed rows by id alone and an upsert could move a contact into the
// caller's tenant.
func TestEveryDirectoryStatementNamesTheTenant(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	var bad []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			sqlText, err := strconv.Unquote(lit.Value)
			if err != nil || reDDL.MatchString(sqlText) || !reDirTable.MatchString(sqlText) {
				return true
			}
			checked++
			if !reTenant.MatchString(sqlText) {
				bad = append(bad, fset.Position(lit.Pos()).String())
			}
			return true
		})
	}
	if checked < 40 {
		t.Fatalf("found only %d directory statements; the scan is broken, not the store", checked)
	}
	if len(bad) > 0 {
		t.Errorf("directory statements without tenant_id:\n  %s", strings.Join(bad, "\n  "))
	}
}
