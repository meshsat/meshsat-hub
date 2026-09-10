package kofi

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/plans"
)

func tenantByID(t *testing.T, st *memTenants, id string) (plan string, expires *time.Time) {
	t.Helper()
	for i := range st.tenants {
		if st.tenants[i].ID == id {
			return st.tenants[i].Plan, st.tenants[i].PlanExpiresAt
		}
	}
	t.Fatalf("no tenant %s", id)
	return "", nil
}

// The hole this closes: the guard used to be `p.MessageID != "" && t.KofiLastMessageID
// == p.MessageID`. Ko-fi does not always send a message_id, and an empty one
// compared against an empty column matched nothing -- so every retry of such a
// delivery bought another 32 days. The receipt was deduped correctly the whole
// time, which is what made it invisible.
func TestReplayWithNoMessageIDGrantsOnlyOneMonth(t *testing.T) {
	st := newStore()
	h := newHandler(st)
	p := Payload{
		VerificationToken: token, IsSubscriptionPayment: true, TierName: "Crew",
		Email: "alice@example.com", Message: "AB2K9XYZ",
		KofiTransactionID: "txn-1", Amount: "9.00", Currency: "EUR",
		// MessageID deliberately absent, as Ko-fi sometimes sends it.
	}
	if rr := post(t, h, p); rr.Code != 200 {
		t.Fatalf("first delivery: %d", rr.Code)
	}
	_, first := tenantByID(t, st, "t-a")
	if first == nil {
		t.Fatal("first delivery granted nothing")
	}

	for i := 0; i < 4; i++ {
		rr := post(t, h, p)
		if rr.Code != 200 {
			t.Fatalf("replay %d: %d", i, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "already applied") {
			t.Fatalf("replay %d was not recognised as a duplicate: %s", i, rr.Body.String())
		}
	}
	_, after := tenantByID(t, st, "t-a")
	if !after.Equal(*first) {
		t.Fatalf("expiry moved on replay: %s -> %s (four replays bought %v)",
			first.Format(time.RFC3339), after.Format(time.RFC3339), after.Sub(*first))
	}
}

// A delivery carrying no message_id used to overwrite KofiLastMessageID with "",
// erasing the guard so the NEXT genuine retry applied twice.
func TestAnIDlessDeliveryDoesNotEraseTheGuard(t *testing.T) {
	st := newStore()
	h := newHandler(st)
	base := Payload{
		VerificationToken: token, IsSubscriptionPayment: true, TierName: "Crew",
		Email: "alice@example.com", Message: "AB2K9XYZ", Amount: "9.00", Currency: "EUR",
	}
	withID := base
	withID.MessageID, withID.KofiTransactionID = "msg-1", "txn-1"
	post(t, h, withID)
	_, afterFirst := tenantByID(t, st, "t-a")

	idless := base
	idless.KofiTransactionID = "txn-2"
	post(t, h, idless) // a different payment, legitimately extends
	_, afterIDless := tenantByID(t, st, "t-a")
	if !afterIDless.After(*afterFirst) {
		t.Fatal("a genuinely different delivery did not extend the plan")
	}

	// Now replay the FIRST one. Its key must still be remembered.
	rr := post(t, h, withID)
	if !strings.Contains(rr.Body.String(), "already applied") {
		t.Fatalf("the first delivery was forgotten: %s", rr.Body.String())
	}
	_, final := tenantByID(t, st, "t-a")
	if !final.Equal(*afterIDless) {
		t.Fatalf("replaying the first delivery extended the plan again: %s -> %s",
			afterIDless.Format(time.RFC3339), final.Format(time.RFC3339))
	}
}

// The webhook is not leader-gated, so both replicas can serve the same retry.
func TestConcurrentDeliveriesOfOneRetryGrantOnce(t *testing.T) {
	st := newStore()
	h := newHandler(st)
	p := Payload{
		VerificationToken: token, IsSubscriptionPayment: true, TierName: "Crew",
		Email: "alice@example.com", Message: "AB2K9XYZ", MessageID: "msg-race",
		KofiTransactionID: "txn-race", Amount: "9.00", Currency: "EUR",
	}
	var wg sync.WaitGroup
	applied := make([]string, 8)
	for i := range applied {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			applied[i] = post(t, h, p).Body.String()
		}(i)
	}
	wg.Wait()

	n := 0
	for _, b := range applied {
		if strings.Contains(b, `"applied"`) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d of 8 concurrent deliveries granted a month; want exactly 1", n)
	}
	_, exp := tenantByID(t, st, "t-a")
	if exp == nil {
		t.Fatal("no plan granted at all")
	}
	if d := time.Until(*exp); d > Period+time.Minute {
		t.Fatalf("plan runs %v out, more than one Period -- a concurrent delivery stacked", d)
	}
}

// A Ko-fi tier is a name somebody types into a web form. It must never be able
// to name an operator-only plan: custom and beta are both unlimited devices.
func TestATierNameCannotBuyAnUnlimitedPlan(t *testing.T) {
	for _, tier := range []string{"custom", "Custom", "beta", "BETA", " custom "} {
		t.Run(tier, func(t *testing.T) {
			st := newStore()
			h := newHandler(st)
			post(t, h, Payload{
				VerificationToken: token, IsSubscriptionPayment: true, TierName: tier,
				Email: "alice@example.com", Message: "AB2K9XYZ", MessageID: "m-" + tier,
				KofiTransactionID: "t-" + tier, Amount: "9.00", Currency: "EUR",
			})
			got, _ := tenantByID(t, st, "t-a")
			if got == plans.Custom || got == plans.Beta {
				t.Fatalf("a Ko-fi tier named %q bought the %s plan (unlimited devices)", tier, got)
			}
			if got != plans.Crew {
				t.Fatalf("tier %q granted %q, want the smallest paid tier", tier, got)
			}
		})
	}
}

// The tiers we do sell must still be reachable by name.
func TestTheSoldTiersAreStillReachableByName(t *testing.T) {
	for tier, want := range map[string]string{"crew": plans.Crew, "Fleet": plans.Fleet} {
		st := newStore()
		h := newHandler(st)
		post(t, h, Payload{
			VerificationToken: token, IsSubscriptionPayment: true, TierName: tier,
			Email: "alice@example.com", Message: "AB2K9XYZ", MessageID: "m-" + tier,
			KofiTransactionID: "t-" + tier, Amount: "9.00", Currency: "EUR",
		})
		if got, _ := tenantByID(t, st, "t-a"); got != want {
			t.Errorf("tier %q granted %q, want %q", tier, got, want)
		}
	}
}
