package stripe

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// The first real subscription was granted "until 2026-10-16" when Stripe's own
// period ended 2026-10-11. Nothing failed: current_period_end had moved off the
// subscription and onto its items in the basil API versions -- which is what
// this endpoint is pinned to -- so periodEnd() saw zero and onSubscription
// silently fell back to now + billing.Period + grace. The plan worked, the
// customer was charged on the right day by Stripe, and every date the HUB held
// was five days out.
//
// That matters because the invented date is what the lapse job acts on, what
// the lapse warning email tells the customer, and what the Settings page shows.
//
// testdata/subscription_created_basil.json is the real subscription off the
// live account, ids replaced, rendered at the pinned version.

func TestThePeriodIsReadFromTheSubscriptionItems(t *testing.T) {
	raw, err := os.ReadFile("testdata/subscription_created_basil.json")
	if err != nil {
		t.Fatalf("reading the captured subscription: %v", err)
	}
	var ev Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	var sub subscription
	if err := json.Unmarshal(ev.Data.Object, &sub); err != nil {
		t.Fatal(err)
	}

	if sub.CurrentPeriodEnd != 0 {
		t.Fatal("the fixture has a top-level current_period_end; it is supposed to prove the field moved")
	}
	got := sub.periodEnd()
	if got.IsZero() {
		t.Fatal("periodEnd() is zero, so the expiry would be invented rather than Stripe's")
	}
	want := time.Date(2026, 10, 11, 3, 5, 9, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("periodEnd() = %s, want %s (the date Stripe actually bills on)",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if sub.priceID() != "price_crew_EXAMPLE" {
		t.Errorf("priceID() = %q", sub.priceID())
	}
}

// End to end: the tenant's expiry is Stripe's period plus the grace, not a
// month-and-a-bit from whenever the webhook happened to arrive.
func TestThePlanExpiresWhenStripeSaysNotWhenWeGuess(t *testing.T) {
	raw, err := os.ReadFile("testdata/subscription_created_basil.json")
	if err != nil {
		t.Fatal(err)
	}
	st := newTenants(store.Tenant{ID: "default", Plan: plans.Free})
	h, _, _ := newHandler(t, st)
	h.SetPrices(map[string]string{"price_crew_EXAMPLE": plans.Crew})

	if w := deliver(t, h, string(raw)); w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	tn, err := st.GetTenant(t.Context(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if tn.Plan != plans.Crew {
		t.Fatalf("plan = %q", tn.Plan)
	}
	if tn.PlanExpiresAt == nil {
		t.Fatal("no expiry was set")
	}
	// Stripe's period end, plus the three days of grace the Hub adds.
	want := time.Date(2026, 10, 11, 3, 5, 9, 0, time.UTC).Add(grace)
	if !tn.PlanExpiresAt.Equal(want) {
		t.Errorf("plan_expires_at = %s, want %s",
			tn.PlanExpiresAt.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	// The bug's signature: an expiry derived from "now" rather than Stripe.
	if tn.PlanExpiresAt.After(h.now().UTC().Add(35 * 24 * time.Hour)) {
		t.Error("the expiry looks invented from the clock rather than read from Stripe")
	}
}

// An event replayed at an older API version still has to work.
func TestTheLegacyTopLevelPeriodStillWorks(t *testing.T) {
	end := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	body := `{"id":"evt_legacy","type":"customer.subscription.created","created":1789095909,"data":{"object":{
		"id":"sub_legacy","customer":"cus_x","status":"active",
		"current_period_end":` + itoa64(end.Unix()) + `,
		"metadata":{"tenant_id":"t-a"},
		"items":{"data":[{"price":{"id":"price_crew"}}]}}}}`

	st := newTenants(store.Tenant{ID: "t-a", Plan: plans.Free})
	h, _, _ := newHandler(t, st)

	if w := deliver(t, h, body); w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	tn, _ := st.GetTenant(t.Context(), "t-a")
	if tn.PlanExpiresAt == nil || !tn.PlanExpiresAt.Equal(end.Add(grace)) {
		t.Errorf("plan_expires_at = %v, want %s", tn.PlanExpiresAt, end.Add(grace))
	}
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }
