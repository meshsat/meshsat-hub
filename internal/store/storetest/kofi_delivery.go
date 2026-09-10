package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// testKofiDelivery pins the plan grant's idempotency against a real database in
// both dialects. The guard has to live here rather than in the handler: the
// Ko-fi webhook is served by every replica, so a read-then-write in Go lets two
// of them both see a stale row and both buy a month for one payment.
func testKofiDelivery(t *testing.T, s store.Store) {
	ctx := context.Background()
	must := func(err error, what string) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	tn := &store.Tenant{ID: "kd-one", Slug: "kd-one", Name: "Payer", Plan: plans.Free, Status: store.TenantActive}
	must(s.CreateTenant(ctx, tn), "create tenant")

	expires := time.Now().UTC().Add(32 * 24 * time.Hour).Truncate(time.Second)
	tn.Plan, tn.PlanExpiresAt, tn.KofiPayerEmail = plans.Crew, &expires, "payer@example.com"

	applied, err := s.ApplyKofiDelivery(ctx, tn, "msg-1")
	must(err, "first delivery")
	if !applied {
		t.Fatal("first delivery did not apply")
	}
	got, err := s.GetTenant(ctx, "kd-one")
	must(err, "read back")
	if got.Plan != plans.Crew || got.PlanExpiresAt == nil {
		t.Fatalf("grant not persisted: plan=%q expires=%v", got.Plan, got.PlanExpiresAt)
	}
	if got.KofiPayerEmail != "payer@example.com" {
		t.Errorf("payer not remembered: %q", got.KofiPayerEmail)
	}

	// A replay must not apply, and must not move the expiry.
	before := *got.PlanExpiresAt
	later := before.Add(32 * 24 * time.Hour)
	tn.PlanExpiresAt = &later
	applied, err = s.ApplyKofiDelivery(ctx, tn, "msg-1")
	must(err, "replay")
	if applied {
		t.Fatal("a replayed delivery applied a second time")
	}
	got, err = s.GetTenant(ctx, "kd-one")
	must(err, "read back after replay")
	if !got.PlanExpiresAt.Equal(before) {
		t.Fatalf("replay moved the expiry: %s -> %s", before, *got.PlanExpiresAt)
	}

	// A genuinely different delivery must apply.
	applied, err = s.ApplyKofiDelivery(ctx, tn, "msg-2")
	must(err, "second delivery")
	if !applied {
		t.Fatal("a new delivery key did not apply")
	}

	// Every key is remembered, not just the last: a retry of msg-1 arriving
	// after msg-2 must still be refused. Remembering only the last one is
	// exactly how a late retry used to buy an extra month.
	applied, err = s.ApplyKofiDelivery(ctx, tn, "msg-1")
	must(err, "late retry of the first delivery")
	if applied {
		t.Fatal("a late retry of an already-applied delivery was applied again")
	}

	// An empty key must be refused outright: an absent id must not become a
	// key that collides with every other absent id.
	if _, err := s.ApplyKofiDelivery(ctx, tn, ""); err == nil {
		t.Fatal("an empty delivery key was accepted")
	}

	// The claim is per-tenant-agnostic but globally unique, so one tenant
	// cannot replay another's delivery key.
	other := &store.Tenant{ID: "kd-two", Slug: "kd-two", Name: "Other", Plan: plans.Free, Status: store.TenantActive}
	must(s.CreateTenant(ctx, other), "create second tenant")
	other.Plan = plans.Fleet
	applied, err = s.ApplyKofiDelivery(ctx, other, "msg-1")
	must(err, "cross-tenant replay")
	if applied {
		t.Fatal("one tenant applied another tenant's delivery key")
	}
	if got, err := s.GetTenant(ctx, "kd-two"); err == nil && got.Plan != plans.Free {
		t.Fatalf("cross-tenant replay changed the plan to %q", got.Plan)
	}
}
