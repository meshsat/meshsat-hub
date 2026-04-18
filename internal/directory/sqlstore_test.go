package directory

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) *SQLStore {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "dir.db")+"?_journal_mode=WAL&_foreign_keys=ON")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	s := NewSQLStore(db)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func TestSQLStore_Migrate_Idempotent(t *testing.T) {
	s := newTestStore(t)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	// Confirm a table actually got created.
	row := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='directory_contacts'`)
	var name string
	if err := row.Scan(&name); err != nil {
		t.Fatalf("directory_contacts missing: %v", err)
	}
}

func TestSQLStore_PutAndGetContact_WithAddresses(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	c := &Contact{
		TenantID:    "acme",
		DisplayName: "Alice Kowalski",
		Team:        "Red",
		Role:        "Medic",
		Origin:      OriginHub,
		Addresses: []Address{
			{Kind: KindSMS, Value: "+31612345678", Label: "Mobile", PrimaryRank: 0},
			{Kind: KindMeshtastic, Value: "!abcd1234", PrimaryRank: 1},
		},
	}
	if err := s.PutContact(ctx, c); err != nil {
		t.Fatalf("PutContact: %v", err)
	}
	if c.ID == "" {
		t.Error("PutContact did not populate ID")
	}

	got, err := s.GetContact(ctx, "acme", c.ID)
	if err != nil {
		t.Fatalf("GetContact: %v", err)
	}
	if got == nil {
		t.Fatal("GetContact returned nil")
	}
	if got.DisplayName != "Alice Kowalski" || got.Team != "Red" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if len(got.Addresses) != 2 {
		t.Fatalf("addresses: got %d, want 2", len(got.Addresses))
	}
	if got.Addresses[0].Kind != KindSMS || got.Addresses[0].Value != "+31612345678" {
		t.Errorf("primary address: %+v", got.Addresses[0])
	}
}

func TestSQLStore_ListAndFindByAddress(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_ = s.PutContact(ctx, &Contact{TenantID: "t1", DisplayName: "Alice",
		Addresses: []Address{{Kind: KindSMS, Value: "+31611111111"}}})
	_ = s.PutContact(ctx, &Contact{TenantID: "t1", DisplayName: "Bob",
		Addresses: []Address{{Kind: KindMeshtastic, Value: "!bob0001"}}})
	_ = s.PutContact(ctx, &Contact{TenantID: "t2", DisplayName: "Charlie",
		Addresses: []Address{{Kind: KindSMS, Value: "+31622222222"}}})

	t1, err := s.ListContacts(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(t1) != 2 {
		t.Errorf("t1 contacts: got %d, want 2", len(t1))
	}

	// Tenant isolation: t2 address must not be reachable from t1.
	gotT1, err := s.FindContactByAddress(ctx, "t1", KindSMS, "+31622222222")
	if err != nil {
		t.Fatal(err)
	}
	if gotT1 != nil {
		t.Errorf("tenant leak: t2 address resolved under t1")
	}

	// Correct tenant → found.
	gotT2, err := s.FindContactByAddress(ctx, "t2", KindSMS, "+31622222222")
	if err != nil {
		t.Fatal(err)
	}
	if gotT2 == nil || gotT2.DisplayName != "Charlie" {
		t.Errorf("find t2: got %+v", gotT2)
	}

	// Unknown address → nil.
	gotNone, err := s.FindContactByAddress(ctx, "t1", KindSMS, "+31699999999")
	if err != nil || gotNone != nil {
		t.Errorf("unknown address should return nil: %+v / err=%v", gotNone, err)
	}

	// Invalid kind → error.
	if _, err := s.FindContactByAddress(ctx, "t1", "BOGUS", "x"); !errors.Is(err, ErrInvalidAddressKind) {
		t.Errorf("invalid kind: err=%v", err)
	}
}

func TestSQLStore_DeleteContact_Cascades(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	c := &Contact{TenantID: "t", DisplayName: "X",
		Addresses: []Address{{Kind: KindSMS, Value: "+31600000000"}}}
	_ = s.PutContact(ctx, c)

	if err := s.DeleteContact(ctx, "t", c.ID); err != nil {
		t.Fatal(err)
	}
	// Re-get → nil.
	got, _ := s.GetContact(ctx, "t", c.ID)
	if got != nil {
		t.Error("deleted contact still retrievable")
	}
	// Addresses cascaded away.
	gotByAddr, _ := s.FindContactByAddress(ctx, "t", KindSMS, "+31600000000")
	if gotByAddr != nil {
		t.Error("address survived contact delete")
	}
	// Delete missing → ErrNotFound.
	if err := s.DeleteContact(ctx, "t", "nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing delete: err=%v", err)
	}
}

func TestSQLStore_GroupAndMembers(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	c1 := &Contact{TenantID: "t", DisplayName: "M1"}
	c2 := &Contact{TenantID: "t", DisplayName: "M2"}
	_ = s.PutContact(ctx, c1)
	_ = s.PutContact(ctx, c2)

	g := &Group{TenantID: "t", Name: "Red Team", Kind: "TEAM", MemberIDs: []string{c1.ID, c2.ID}}
	if err := s.PutGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetGroup(ctx, "t", g.ID)
	if err != nil || got == nil {
		t.Fatalf("get group: %+v / err=%v", got, err)
	}
	if len(got.MemberIDs) != 2 {
		t.Errorf("members: got %d, want 2", len(got.MemberIDs))
	}

	groups, err := s.ListGroups(ctx, "t")
	if err != nil || len(groups) != 1 {
		t.Errorf("list groups: %+v / err=%v", groups, err)
	}

	if err := s.DeleteGroup(ctx, "t", g.ID); err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetGroup(ctx, "t", g.ID)
	if after != nil {
		t.Error("group not deleted")
	}
}

func TestSQLStore_PolicyRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := WithTenant(context.Background(), "t")

	p := &DispatchPolicy{
		Name:      "Flash-bonded",
		ScopeType: "precedence",
		ScopeID:   "Flash",
		Strategy:  StrategyHeMBBonded,
		Preferred: []AddressKind{KindMeshtastic, KindSMS},
	}
	if err := s.PutPolicy(ctx, p); err != nil {
		t.Fatalf("PutPolicy: %v", err)
	}
	got, err := s.GetPolicy(ctx, "t", p.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	if got.Strategy != StrategyHeMBBonded || len(got.Preferred) != 2 {
		t.Errorf("policy round-trip mismatch: %+v", got)
	}

	list, _ := s.ListPolicies(ctx, "t")
	if len(list) != 1 {
		t.Errorf("list policies: %d", len(list))
	}

	if err := s.DeletePolicy(ctx, "t", p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePolicy(ctx, "t", "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing delete: err=%v", err)
	}
}

func TestSQLStore_VersionAndSnapshot(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	v0, err := s.CurrentVersion(ctx, "t")
	if err != nil || v0 != 0 {
		t.Errorf("initial version: v=%d err=%v", v0, err)
	}
	v1, err := s.BumpVersion(ctx, "t")
	if err != nil || v1 != 1 {
		t.Errorf("bump1: v=%d err=%v", v1, err)
	}
	v2, _ := s.BumpVersion(ctx, "t")
	if v2 != 2 {
		t.Errorf("bump2: v=%d", v2)
	}
	vCur, _ := s.CurrentVersion(ctx, "t")
	if vCur != 2 {
		t.Errorf("current after two bumps: %d", vCur)
	}

	_ = s.PutContact(ctx, &Contact{TenantID: "t", DisplayName: "One",
		Addresses: []Address{{Kind: KindSMS, Value: "+31600111111"}}})

	snap, err := s.Snapshot(ctx, "t")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.TenantID != "t" {
		t.Errorf("tenant: %s", snap.TenantID)
	}
	if snap.Version != 2 {
		t.Errorf("snap version: %d", snap.Version)
	}
	if len(snap.Contacts) != 1 {
		t.Errorf("snap contacts: %d", len(snap.Contacts))
	}
}

func TestSQLStore_EmptyIDGuards(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.GetContact(ctx, "", "x"); !errors.Is(err, ErrEmptyID) {
		t.Errorf("empty tenant: %v", err)
	}
	if _, err := s.ListContacts(ctx, ""); !errors.Is(err, ErrEmptyID) {
		t.Errorf("empty list: %v", err)
	}
	if _, err := s.BumpVersion(ctx, ""); !errors.Is(err, ErrEmptyID) {
		t.Errorf("empty bump: %v", err)
	}
}

// --- MESHSAT-547 S2-04 precedence defaults ---------------------------

func TestSeedPrecedenceDefaults(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	inserted, err := s.SeedPrecedenceDefaults(ctx, "acme")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if inserted != 7 {
		t.Errorf("first-run insert count: got %d, want 7", inserted)
	}

	// Second call is a no-op.
	inserted2, err := s.SeedPrecedenceDefaults(ctx, "acme")
	if err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if inserted2 != 0 {
		t.Errorf("second-run should insert 0, got %d", inserted2)
	}

	// Policies are retrievable with the expected strategies.
	want := map[string]string{
		"Override":  "HEMB_BONDED",
		"Flash":     "HEMB_BONDED",
		"Immediate": "ANY_REACHABLE",
		"Priority":  "ORDERED_FALLBACK",
		"Routine":   "PRIMARY_ONLY",
		"Deferred":  "PRIMARY_ONLY",
	}
	list, err := s.ListPolicies(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range list {
		if p.ScopeType == "precedence" {
			got[p.ScopeID] = string(p.Strategy)
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("precedence %s: got %q, want %q", k, got[k], v)
		}
	}

	// Tenant isolation: a different tenant starts empty, then seeds.
	list2, _ := s.ListPolicies(ctx, "other")
	if len(list2) != 0 {
		t.Errorf("tenant 'other' should start empty, got %d", len(list2))
	}
	_, _ = s.SeedPrecedenceDefaults(ctx, "other")
	list2, _ = s.ListPolicies(ctx, "other")
	if len(list2) != 7 {
		t.Errorf("tenant 'other' after seed: got %d, want 7", len(list2))
	}
}

func TestSeedPrecedenceDefaults_EmptyTenant(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.SeedPrecedenceDefaults(context.Background(), ""); err == nil {
		t.Error("empty tenant: expected error")
	}
}
