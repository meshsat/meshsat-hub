package audit

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

func testAuditService(t *testing.T) *Service {
	t.Helper()
	db, err := sqlite.New(filepath.Join(t.TempDir(), "test.db"), 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db)
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
