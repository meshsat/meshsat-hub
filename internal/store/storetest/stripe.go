package storetest

import (
	"context"
	"sync"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// testStripe pins the payment provider's two guarantees against a real database
// in both dialects (MESHSAT-1023).
//
// Both have to live here rather than in Go: the webhook is served by every
// replica, and Stripe redelivers an event until it gets a 2xx. A read followed
// by a write lets two replicas both find a retry missing and both apply it —
// which for a subscription event means buying a period twice, and for a refund
// event means two credit notes out of a gapless series.
func testStripe(t *testing.T, s store.Store) {
	ctx := context.Background()
	must := func(err error, what string) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	tn := &store.Tenant{ID: "st-one", Slug: "st-one", Name: "Subscriber", Plan: plans.Free, Status: store.TenantActive}
	must(s.CreateTenant(ctx, tn), "create tenant")

	// --- the customer binding survives a round trip ------------------------
	tn.StripeCustomerID = "cus_ABC"
	tn.StripeSubscriptionID = "sub_ABC"
	must(s.UpdateTenant(ctx, tn), "bind the customer")

	got, err := s.GetTenant(ctx, "st-one")
	must(err, "read back")
	if got.StripeCustomerID != "cus_ABC" || got.StripeSubscriptionID != "sub_ABC" {
		t.Fatalf("the binding did not persist: %+v", got)
	}

	// --- and is how an event carrying only a customer finds its tenant -----
	byCus, err := s.TenantByStripeCustomer(ctx, "cus_ABC")
	must(err, "resolve by customer")
	if byCus == nil || byCus.ID != "st-one" {
		t.Fatalf("TenantByStripeCustomer returned %+v", byCus)
	}
	// An unknown customer is not somebody else's tenant.
	if other, err := s.TenantByStripeCustomer(ctx, "cus_NOBODY"); err == nil && other != nil {
		t.Fatalf("an unknown customer resolved to tenant %q", other.ID)
	}
	// Nor is an empty one, which is what an event with no customer carries.
	if other, err := s.TenantByStripeCustomer(ctx, ""); err == nil && other != nil {
		t.Fatalf("an empty customer id resolved to tenant %q", other.ID)
	}

	// --- the event guard is a compare-and-set ------------------------------
	applied, err := s.ApplyStripeEvent(ctx, "evt_1", "st-one")
	must(err, "first event")
	if !applied {
		t.Fatal("the first delivery of an event did not apply")
	}
	for i := 0; i < 3; i++ {
		again, err := s.ApplyStripeEvent(ctx, "evt_1", "st-one")
		must(err, "redelivery")
		if again {
			t.Fatal("a redelivered event applied twice; Stripe retries until it gets a 2xx")
		}
	}
	// A different event still applies, so the guard is not just refusing.
	applied, err = s.ApplyStripeEvent(ctx, "evt_2", "st-one")
	must(err, "second event")
	if !applied {
		t.Fatal("a second, distinct event was refused")
	}

	// --- and it holds under concurrency, which is the case it exists for ---
	const racers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			ok, err := s.ApplyStripeEvent(context.Background(), "evt_race", "st-one")
			if err != nil {
				return
			}
			if ok {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("%d of %d concurrent deliveries applied; exactly one may win, "+
			"or one payment buys several periods", winners, racers)
	}

	// An event id is required: an empty one would collide with every other
	// empty one and dedupe unrelated payments into a single application.
	if _, err := s.ApplyStripeEvent(ctx, "", "st-one"); err == nil {
		t.Error("an empty event id was accepted")
	}
}
