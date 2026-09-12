package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// testHostedTAK holds both stores to one contract for the hosted-TAK tables
// (MESHSAT-1037).
//
// The interesting parts are the ones where the two stores are genuinely
// different and could drift silently:
//
//   - Postgres has real TIMESTAMPTZ and NULL; sqlite stores times as TEXT in
//     columns declared NOT NULL with an empty-string default. So "this user has
//     no certificate yet" is NULL in one and a zero-length string in the other,
//     and both must read
//     back as a nil pointer. Getting that wrong turns "never enrolled" into "the
//     enrollment window closed in year 1", which looks like a real expired
//     deadline.
//   - Both must refuse a second user with the same name in the same tenant, from
//     the primary key rather than from a read-then-write that two replicas could
//     both pass.
func testHostedTAK(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, id := range []string{"tak-a", "tak-b"} {
		if err := s.CreateTenant(ctx, &store.Tenant{ID: id, Slug: id, Name: id, Status: store.TenantActive}); err != nil {
			t.Fatalf("create tenant %s: %v", id, err)
		}
	}

	// --- the instance row -------------------------------------------------
	if _, err := s.GetTAKInstance(ctx, "tak-a"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a tenant with no instance gave %v, want ErrNotFound", err)
	}

	inst := &store.TAKInstance{
		TenantID: "tak-a", Label: "abcdefghij", State: "Running",
		Phase: "Provisioning", Host: "tak-abcdefghij.meshsat-tak.svc:8089",
		CACertPEM: "-----BEGIN CERTIFICATE-----\nnot a real one\n-----END CERTIFICATE-----\n",
	}
	if err := s.UpsertTAKInstance(ctx, inst); err != nil {
		t.Fatalf("upsert instance: %v", err)
	}
	got, err := s.GetTAKInstance(ctx, "tak-a")
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if got.Label != "abcdefghij" || got.Phase != "Provisioning" {
		t.Errorf("instance round trip lost fields: %+v", got)
	}
	if got.CACertPEM != inst.CACertPEM {
		t.Error("the CA certificate did not survive the round trip; takfront cannot resolve a tenant without it")
	}

	// Upsert is how the operator reports a phase change: the same tenant must
	// land on the same row rather than accumulating history.
	inst.Phase = "Ready"
	if err := s.UpsertTAKInstance(ctx, inst); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if got, err = s.GetTAKInstance(ctx, "tak-a"); err != nil || got.Phase != "Ready" {
		t.Errorf("after re-upsert: phase=%q err=%v, want Ready", got.Phase, err)
	}
	all, err := s.ListTAKInstances(ctx)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("listed %d instances, want 1 (upsert must not insert a second row)", len(all))
	}

	// --- users ------------------------------------------------------------
	if n, err := s.CountTAKUsers(ctx, "tak-a"); err != nil || n != 0 {
		t.Fatalf("empty tenant: %d, %v; want 0", n, err)
	}

	expiry := time.Now().Add(90 * 24 * time.Hour).UTC().Truncate(time.Second)
	users := []store.TAKUser{
		// One fully enrolled, with both timestamps set.
		{Username: "alice", Callsign: "ALICE", Active: true,
			CertSerial: "1234", CertNotAfter: &expiry},
		// One invited and not yet enrolled: NO certificate and no expiry, which
		// is the case the two stores represent differently.
		{Username: "bob", Callsign: "BOB", Active: true,
			EnrollTokenHash: "abc123", EnrollExpiresAt: nil},
	}
	for i := range users {
		if err := s.CreateTAKUser(ctx, "tak-a", &users[i]); err != nil {
			t.Fatalf("create user %s: %v", users[i].Username, err)
		}
	}
	if n, err := s.CountTAKUsers(ctx, "tak-a"); err != nil || n != 2 {
		t.Errorf("TAK users = %d, %v; want 2", n, err)
	}

	// The un-enrolled user's absent times must read back as NIL, not as the zero
	// instant. This is the assertion that catches the sqlite empty-string case.
	bob, err := s.GetTAKUser(ctx, "tak-a", "bob")
	if err != nil {
		t.Fatalf("get bob: %v", err)
	}
	if bob.CertNotAfter != nil {
		t.Errorf("bob has no certificate but CertNotAfter = %v; an absent time must be nil, "+
			"or a never-enrolled user looks like one whose certificate expired long ago", bob.CertNotAfter)
	}
	if bob.EnrollExpiresAt != nil {
		t.Errorf("bob's EnrollExpiresAt = %v, want nil", bob.EnrollExpiresAt)
	}
	if bob.EnrollTokenHash != "abc123" {
		t.Errorf("enroll token hash = %q, want it stored (hashed, never the token itself)", bob.EnrollTokenHash)
	}

	// And the enrolled user's expiry must survive to the second.
	alice, err := s.GetTAKUser(ctx, "tak-a", "alice")
	if err != nil {
		t.Fatalf("get alice: %v", err)
	}
	if alice.CertNotAfter == nil {
		t.Fatal("alice's certificate expiry was lost")
	}
	if d := alice.CertNotAfter.Sub(expiry); d > time.Second || d < -time.Second {
		t.Errorf("certificate expiry drifted by %v (stored %v, read %v)", d, expiry, alice.CertNotAfter)
	}

	// A duplicate username in the same tenant is refused by the key.
	dup := store.TAKUser{Username: "alice", Active: true}
	if err := s.CreateTAKUser(ctx, "tak-a", &dup); err == nil {
		t.Error("a duplicate username was accepted; (tenant_id, username) must be unique")
	}

	// --- deactivation and revocation keep the row ------------------------
	alice.Active = false
	alice.RevokedSerial = alice.CertSerial
	if err := s.UpdateTAKUser(ctx, "tak-a", alice); err != nil {
		t.Fatalf("update alice: %v", err)
	}
	if alice, err = s.GetTAKUser(ctx, "tak-a", "alice"); err != nil {
		t.Fatalf("re-get alice: %v", err)
	}
	if alice.Active {
		t.Error("alice is still active after being deactivated")
	}
	if alice.RevokedSerial != "1234" {
		t.Errorf("revoked serial = %q, want 1234; the Authorizer refuses on this", alice.RevokedSerial)
	}
	// A deactivated user still counts: the row exists and the seat is taken.
	if n, _ := s.CountTAKUsers(ctx, "tak-a"); n != 2 {
		t.Errorf("after deactivation count = %d, want 2 (the row is kept)", n)
	}
	if err := s.UpdateTAKUser(ctx, "tak-a", &store.TAKUser{Username: "nobody"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("updating a missing user gave %v, want ErrNotFound", err)
	}

	// --- tenant isolation -------------------------------------------------
	if err := s.CreateTAKUser(ctx, "tak-b", &store.TAKUser{Username: "alice", Active: true}); err != nil {
		t.Fatalf("the same username in ANOTHER tenant must be allowed: %v", err)
	}
	if n, _ := s.CountTAKUsers(ctx, "tak-b"); n != 1 {
		t.Errorf("neighbour count = %d, want 1", n)
	}
	if n, _ := s.CountTAKUsers(ctx, "tak-a"); n != 2 {
		t.Errorf("the neighbour's user leaked into this tenant's count: %d, want 2", n)
	}
	if list, err := s.ListTAKUsers(ctx, "tak-b"); err != nil || len(list) != 1 {
		t.Errorf("list for the neighbour = %d users, %v; want 1", len(list), err)
	}
	if _, err := s.GetTAKUser(ctx, "tak-b", "bob"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("reading another tenant's user gave %v, want ErrNotFound", err)
	}

	// --- removal frees the seat ------------------------------------------
	if err := s.DeleteTAKUser(ctx, "tak-a", "bob"); err != nil {
		t.Fatalf("delete bob: %v", err)
	}
	if n, _ := s.CountTAKUsers(ctx, "tak-a"); n != 1 {
		t.Errorf("after delete count = %d, want 1", n)
	}
	if err := s.DeleteTAKInstance(ctx, "tak-a"); err != nil {
		t.Fatalf("delete instance: %v", err)
	}
	if _, err := s.GetTAKInstance(ctx, "tak-a"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("instance after delete gave %v, want ErrNotFound", err)
	}
}
