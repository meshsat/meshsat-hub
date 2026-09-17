package audit

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

func testAuditService(t *testing.T) *Service {
	t.Helper()
	svc, _ := testAuditServiceAt(t)
	return svc
}

// testAuditServiceAt also hands back the SQLite file, for tests that need to
// edit rows behind the store's back.
func testAuditServiceAt(t *testing.T) (*Service, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlite.New(path, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db), path
}

func TestLog_CreatesChain(t *testing.T) {
	svc := testAuditService(t)
	ctx := context.Background()
	tid := "test-tenant"

	// First entry — prev_hash should be empty.
	if err := svc.Log(ctx, tid, "device_created", "user-1", "IMEI 123", "10.0.0.1"); err != nil {
		t.Fatalf("log 1: %v", err)
	}

	// Second entry — prev_hash should be the first entry's hash.
	if err := svc.Log(ctx, tid, "message_sent", "user-1", "MT to 123", "10.0.0.1"); err != nil {
		t.Fatalf("log 2: %v", err)
	}

	entries, err := svc.store.ListAuditEntries(ctx, tid, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// Entries are newest-first.
	second := entries[0]
	first := entries[1]

	if first.PrevHash != "" {
		t.Errorf("first entry prev_hash should be empty, got %q", first.PrevHash)
	}
	if first.Hash == "" {
		t.Error("first entry hash should not be empty")
	}
	if second.PrevHash != first.Hash {
		t.Errorf("second.prev_hash (%q) != first.hash (%q)", second.PrevHash, first.Hash)
	}
	if second.Hash == "" {
		t.Error("second entry hash should not be empty")
	}
}

func TestVerifyChain_ValidChain(t *testing.T) {
	svc := testAuditService(t)
	ctx := context.Background()
	tid := "test-tenant"

	for i := 0; i < 5; i++ {
		if err := svc.Log(ctx, tid, "test_action", "user-1", "detail", ""); err != nil {
			t.Fatalf("log %d: %v", i, err)
		}
	}

	verified, broken, err := svc.VerifyChain(ctx, tid)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if broken != nil {
		t.Errorf("chain should be valid, broken at entry %d", verified)
	}
	if verified != 5 {
		t.Errorf("expected 5 verified, got %d", verified)
	}
}

func TestVerifyChain_EmptyLog(t *testing.T) {
	svc := testAuditService(t)
	ctx := context.Background()

	verified, broken, err := svc.VerifyChain(ctx, "empty-tenant")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if broken != nil {
		t.Error("empty log should verify successfully")
	}
	if verified != 0 {
		t.Errorf("expected 0 verified, got %d", verified)
	}
}

func TestVerifyChain_PurgedHead(t *testing.T) {
	// Retention deletes the oldest entries; the surviving segment must still
	// verify (production: 39 entries whose head linked to a purged predecessor).
	svc := testAuditService(t)
	ctx := context.Background()
	tid := "purged-tenant"
	for i := 0; i < 5; i++ {
		if err := svc.Log(ctx, tid, "test_action", "user-1", "detail", ""); err != nil {
			t.Fatalf("log %d: %v", i, err)
		}
		if i == 0 {
			time.Sleep(1100 * time.Millisecond) // stores compare whole seconds; the head must sit in an earlier second
		}
	}
	all, err := svc.store.ListAuditEntries(ctx, tid, 0)
	if err != nil || len(all) != 5 {
		t.Fatalf("list: %v %d", err, len(all))
	}
	oldest := all[len(all)-1]
	if n, err := svc.store.DeleteAuditEntriesBefore(ctx, tid, oldest.CreatedAt.Add(time.Second)); err != nil || n != 1 {
		t.Fatalf("purge: %v %d", err, n)
	}
	verified, broken, err := svc.VerifyChain(ctx, tid)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if broken != nil {
		t.Fatalf("purged head must not break the chain (broken at %s)", broken.ID)
	}
	if verified != 4 {
		t.Errorf("expected 4 verified, got %d", verified)
	}
}

func TestVerifyChain_TamperedEntry(t *testing.T) {
	svc := testAuditService(t)
	ctx := context.Background()
	tid := "test-tenant"

	_ = svc.Log(ctx, tid, "action_1", "user-1", "ok", "")
	_ = svc.Log(ctx, tid, "action_2", "user-1", "ok", "")
	_ = svc.Log(ctx, tid, "action_3", "user-1", "ok", "")

	// Tamper: insert a rogue entry with wrong prev_hash.
	rogue := &store.AuditEntry{
		Action:   "rogue",
		Actor:    "attacker",
		Detail:   "tampered",
		PrevHash: "0000000000000000000000000000000000000000000000000000000000000000",
		Hash:     ComputeHash(&store.AuditEntry{Action: "rogue", Actor: "attacker", Detail: "tampered", PrevHash: "0000000000000000000000000000000000000000000000000000000000000000"}),
	}
	_ = svc.store.InsertAuditEntry(ctx, tid, rogue)

	verified, broken, err := svc.VerifyChain(ctx, tid)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if broken == nil {
		t.Fatal("chain should be broken after tampering")
	}
	if verified != 3 {
		t.Errorf("expected break at entry 3, got %d", verified)
	}
}

func TestComputeHash_Deterministic(t *testing.T) {
	e := &store.AuditEntry{
		Action:   "test",
		Actor:    "user",
		Detail:   "detail",
		IP:       "1.2.3.4",
		PrevHash: "abc123",
	}
	h1 := ComputeHash(e)
	h2 := ComputeHash(e)
	if h1 != h2 {
		t.Error("hash should be deterministic")
	}
	if len(h1) != 64 {
		t.Errorf("hash length: got %d, want 64", len(h1))
	}
}

func TestTenantIsolation(t *testing.T) {
	svc := testAuditService(t)
	ctx := context.Background()

	_ = svc.Log(ctx, "tenant-a", "action_a", "user-a", "", "")
	_ = svc.Log(ctx, "tenant-b", "action_b", "user-b", "", "")

	// Each tenant's chain should be independent and valid.
	vA, brokenA, _ := svc.VerifyChain(ctx, "tenant-a")
	vB, brokenB, _ := svc.VerifyChain(ctx, "tenant-b")

	if brokenA != nil || vA != 1 {
		t.Errorf("tenant-a: verified=%d, broken=%v", vA, brokenA)
	}
	if brokenB != nil || vB != 1 {
		t.Errorf("tenant-b: verified=%d, broken=%v", vB, brokenB)
	}
}

// --- hash_version 2: what the digest binds, and what it must NOT tolerate ---

// rawDB opens the test's SQLite file a second time so a test can do what an
// attacker with database access would do: UPDATE a row in place.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqlite.DSN(path))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestLog_WritesVersion2WithIdentityAndClock(t *testing.T) {
	svc := testAuditService(t)
	ctx := context.Background()
	tid := "v2-tenant"
	for i := 0; i < 3; i++ {
		if err := svc.Log(ctx, tid, "a", "u", "d|with|pipes", "1.2.3.4"); err != nil {
			t.Fatalf("log %d: %v", i, err)
		}
	}
	entries, err := svc.store.ListAuditEntries(ctx, tid, 0)
	if err != nil || len(entries) != 3 {
		t.Fatalf("list: %v %d", err, len(entries))
	}
	for i, e := range entries {
		if e.HashVersion != HashVersionCurrent {
			t.Errorf("entry %d: hash_version %d, want %d", i, e.HashVersion, HashVersionCurrent)
		}
		if e.CreatedAt.IsZero() || e.ID == "" {
			t.Errorf("entry %d: id %q created_at %v — both must be fixed before hashing", i, e.ID, e.CreatedAt)
		}
		if e.Hash != ComputeHashV2(tid, &entries[i]) {
			t.Errorf("entry %d: stored hash does not recompute from the stored row; created_at did not round-trip", i)
		}
	}
	// newest-first: the clock is strictly monotonic within a tenant's chain.
	for i := 0; i+1 < len(entries); i++ {
		if !entries[i].CreatedAt.After(entries[i+1].CreatedAt) {
			t.Errorf("created_at not strictly increasing: %v then %v", entries[i+1].CreatedAt, entries[i].CreatedAt)
		}
	}
}

// Every mutation here breaks a v2 chain. The negative control at the end
// shows the same mutations pass a v1 chain untouched — that gap is the
// reason v2 exists, and the control is what proves v2 closes it rather than
// the test merely asserting it.
func TestVerifyChain_V2DetectsMovedAndBackdatedRows(t *testing.T) {
	type tamper struct {
		name string
		sql  string
	}
	tampers := []tamper{
		{"back-date", "UPDATE audit_log SET created_at = '2001-01-01 00:00:00.000000' WHERE id = ?"},
		{"re-identify", "UPDATE audit_log SET id = id || '-x' WHERE id = ?"},
	}
	for _, tp := range tampers {
		t.Run("v2/"+tp.name, func(t *testing.T) {
			svc, path := testAuditServiceAt(t)
			ctx := context.Background()
			tid := "victim"
			for i := 0; i < 3; i++ {
				if err := svc.Log(ctx, tid, "act", "u", "", ""); err != nil {
					t.Fatal(err)
				}
			}
			entries, _ := svc.store.ListAuditEntries(ctx, tid, 0)
			target := entries[1].ID // the middle row
			if _, err := rawDB(t, path).ExecContext(ctx, tp.sql, target); err != nil {
				t.Fatalf("tamper: %v", err)
			}
			_, broken, err := svc.VerifyChain(ctx, tid)
			if err != nil {
				t.Fatal(err)
			}
			if broken == nil {
				t.Fatalf("%s: chain still verifies after the row was edited", tp.name)
			}
		})
	}

	t.Run("v2/move-to-another-tenant", func(t *testing.T) {
		svc, path := testAuditServiceAt(t)
		ctx := context.Background()
		for i := 0; i < 2; i++ {
			if err := svc.Log(ctx, "victim", "act", "u", "", ""); err != nil {
				t.Fatal(err)
			}
		}
		if err := svc.Log(ctx, "thief", "act", "u", "", ""); err != nil {
			t.Fatal(err)
		}
		victim, _ := svc.store.ListAuditEntries(ctx, "victim", 0)
		thief, _ := svc.store.ListAuditEntries(ctx, "thief", 0)
		// Re-home the victim's newest row under the thief, re-linking it onto
		// the thief's head so only the tenant binding can give it away.
		if _, err := rawDB(t, path).ExecContext(ctx,
			"UPDATE audit_log SET tenant_id = 'thief', prev_hash = ? WHERE id = ?", thief[0].Hash, victim[0].ID); err != nil {
			t.Fatalf("move: %v", err)
		}
		_, broken, err := svc.VerifyChain(ctx, "thief")
		if err != nil {
			t.Fatal(err)
		}
		if broken == nil {
			t.Fatal("a row moved between tenants still verifies under its new tenant")
		}
	})

	// Negative control: the identical tampers against v1 rows are invisible.
	for _, tp := range tampers {
		t.Run("v1-control/"+tp.name, func(t *testing.T) {
			svc, path := testAuditServiceAt(t)
			ctx := context.Background()
			tid := "legacy"
			prev := ""
			for i := 0; i < 3; i++ {
				e := &store.AuditEntry{Action: "act", Actor: "u", PrevHash: prev, HashVersion: 1}
				e.Hash = ComputeHash(e)
				if err := svc.store.InsertAuditEntry(ctx, tid, e); err != nil {
					t.Fatal(err)
				}
				prev = e.Hash
			}
			entries, _ := svc.store.ListAuditEntries(ctx, tid, 0)
			if _, err := rawDB(t, path).ExecContext(ctx, tp.sql, entries[1].ID); err != nil {
				t.Fatalf("tamper: %v", err)
			}
			if _, broken, _ := svc.VerifyChain(ctx, tid); broken != nil {
				t.Fatalf("control: v1 was never expected to notice %s; if it does, the control no longer proves what v2 adds", tp.name)
			}
		})
	}
}

// A legacy-formula row after a v2 row is a downgrade, not history. The row
// here is otherwise perfect: correctly linked, correctly hashed the v1 way.
func TestVerifyChain_LegacyRowAfterV2IsABreak(t *testing.T) {
	svc := testAuditService(t)
	ctx := context.Background()
	tid := "t"
	if err := svc.Log(ctx, tid, "act", "u", "", ""); err != nil {
		t.Fatal(err)
	}
	head, _ := svc.store.GetLatestAuditEntry(ctx, tid)
	e := &store.AuditEntry{Action: "act", Actor: "u", PrevHash: head.Hash, HashVersion: 1}
	e.Hash = ComputeHash(e)
	if err := svc.store.InsertAuditEntry(ctx, tid, e); err != nil {
		t.Fatal(err)
	}
	verified, broken, err := svc.VerifyChain(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	if broken == nil || verified != 1 {
		t.Fatalf("v1 row after a v2 row must break the chain at 1; verified=%d broken=%v", verified, broken)
	}
}

// Legacy rows at the HEAD of a chain (everything written before migration 30)
// stay valid, and the first v2 row chains onto the last of them.
func TestVerifyChain_LegacyHeadThenV2Tail(t *testing.T) {
	svc := testAuditService(t)
	ctx := context.Background()
	tid := "migrated"
	prev := ""
	for i := 0; i < 3; i++ {
		e := &store.AuditEntry{Action: "old", Actor: "u", PrevHash: prev, HashVersion: 1}
		e.Hash = ComputeHash(e)
		if err := svc.store.InsertAuditEntry(ctx, tid, e); err != nil {
			t.Fatal(err)
		}
		prev = e.Hash
	}
	for i := 0; i < 2; i++ {
		if err := svc.Log(ctx, tid, "new", "u", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	verified, broken, err := svc.VerifyChain(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	if broken != nil || verified != 5 {
		t.Fatalf("mixed chain must verify all 5; verified=%d broken=%v", verified, broken)
	}
}

// Two children of one parent cannot both be stored: the unique index on
// (tenant_id, prev_hash) refuses the second, whatever wrote it.
func TestInsert_ForkIsRefused(t *testing.T) {
	svc := testAuditService(t)
	ctx := context.Background()
	tid := "t"
	if err := svc.Log(ctx, tid, "act", "u", "", ""); err != nil {
		t.Fatal(err)
	}
	head, _ := svc.store.GetLatestAuditEntry(ctx, tid)
	a := &store.AuditEntry{Action: "child-a", PrevHash: head.Hash, HashVersion: 1}
	a.Hash = ComputeHash(a)
	if err := svc.store.InsertAuditEntry(ctx, tid, a); err != nil {
		t.Fatalf("first child: %v", err)
	}
	b := &store.AuditEntry{Action: "child-b", PrevHash: head.Hash, HashVersion: 1}
	b.Hash = ComputeHash(b)
	if err := svc.store.InsertAuditEntry(ctx, tid, b); err == nil {
		t.Fatal("a second child of the same parent was accepted; the chain can fork")
	}
	// ... and a different tenant may of course start from the same empty root.
	if err := svc.Log(ctx, "other", "act", "u", "", ""); err != nil {
		t.Fatalf("another tenant's first entry: %v", err)
	}
}
