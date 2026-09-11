package stripe

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/billing"
	"github.com/meshsat/meshsat-hub/internal/metrics"
	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// donationEvent builds a completed one-off Checkout session. marker controls
// whether the Hub's own metadata is present, which is the only thing that
// separates an anonymous gift from a payment that lost its tenant.
func donationEvent(id, tenant, country string, cents int64, marker bool) string {
	meta := ""
	switch {
	case tenant != "" && marker:
		meta = fmt.Sprintf(`{"tenant_id":%q,%q:%q}`, tenant, MetadataKind, KindDonation)
	case tenant != "":
		meta = fmt.Sprintf(`{"tenant_id":%q}`, tenant)
	case marker:
		meta = fmt.Sprintf(`{%q:%q}`, MetadataKind, KindDonation)
	default:
		meta = `{}`
	}
	return fmt.Sprintf(`{"id":%q,"type":%q,"created":%d,"data":{"object":{
		"id":"cs_anon","payment_intent":"pi_anon","mode":"payment",
		"amount_total":%d,"currency":"eur","metadata":%s,
		"customer_details":{"email":"giver@example.com","name":"A Giver","address":{"country":%q}}}}}`,
		id, EventCheckoutCompleted, fixedNow.Unix(), cents, meta, country)
}

// Somebody with no account presses Donate on the website. Before this the
// webhook reached tenantFor, failed, and filed the money as unattributable --
// so a gift produced no document and landed in a list a person could not
// resolve, because there was no account to attach it to.
func TestAnAnonymousDonationIsRecordedAgainstThePlatformTenant(t *testing.T) {
	st := newTenants() // no tenants at all
	h, rc, _ := newHandler(t, st)

	if w := deliver(t, h, donationEvent("evt_anon", "", "NL", 500, true)); w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if len(rc.rows) != 1 {
		t.Fatalf("receipts = %d, want 1: a gift with no account still needs a document", len(rc.rows))
	}
	r := rc.rows[0]
	if r.TenantID != store.DefaultTenantID {
		t.Errorf("tenant = %q, want the platform tenant: a receipt row is tenant-scoped and there is no other tenant it could belong to", r.TenantID)
	}
	if r.Plan != billing.DonationPlan {
		t.Errorf("plan = %q, want %q", r.Plan, billing.DonationPlan)
	}
	// Everything identifying the giver comes from what Stripe collected, which
	// is also what the document is made out to.
	if r.Email != "giver@example.com" || r.Name != "A Giver" || r.Country != "NL" {
		t.Errorf("giver details lost: %+v", r)
	}
	if r.AmountCents != 500 {
		t.Errorf("amount = %d, want 500", r.AmountCents)
	}
}

// The marker is load-bearing. Without it an anonymous gift and a payment that
// lost its tenant are the same event, and one of them would have to be guessed
// at. A session the Hub did not stamp stays unattributed.
func TestAOneOffPaymentWithNoTenantAndNoMarkerIsStillUnattributed(t *testing.T) {
	st := newTenants()
	h, rc, _ := newHandler(t, st)

	if w := deliver(t, h, donationEvent("evt_stray", "", "NL", 500, false)); w.Code != 200 {
		t.Fatalf("got %d: %s (an unattributable payment is acknowledged, not retried)", w.Code, w.Body.String())
	}
	if len(rc.rows) != 0 {
		t.Fatalf("receipts = %d, want 0: an unmarked payment must not be invented into a donation", len(rc.rows))
	}
}

// A session created before the marker existed must still produce a document if
// it is paid after the deploy. Requiring the marker on the attributed path
// would mean money taken with no receipt, which is the one outcome the outbox
// exists to prevent.
func TestAnOlderDonationWithATenantButNoMarkerStillGetsAReceipt(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1", Plan: plans.Free})
	h, rc, _ := newHandler(t, st)

	if w := deliver(t, h, donationEvent("evt_old", "t1", "NL", 900, false)); w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if len(rc.rows) != 1 {
		t.Fatalf("receipts = %d, want 1", len(rc.rows))
	}
	if got := st.get("t1"); got.Plan != plans.Free {
		t.Errorf("a donation bought a tier: %q", got.Plan)
	}
}

// A donation grants nothing, here or anywhere. This is not a nicety: it is the
// condition that makes the money outside the scope of BTW. A donation that
// unlocked something would acquire a counter-performance and become a sale at
// 21% -- see internal/vat.ForDonation.
func TestADonationNeverGrantsAPlan(t *testing.T) {
	for _, tc := range []struct{ name, tenant string }{
		{"anonymous", ""},
		{"signed in", "t1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newTenants(store.Tenant{ID: "t1", Plan: plans.Free})
			h, _, _ := newHandler(t, st)
			if w := deliver(t, h, donationEvent("evt_"+tc.name, tc.tenant, "NL", 50000, true)); w.Code != 200 {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
			if got := st.get("t1"); got.Plan != plans.Free {
				t.Errorf("plan = %q; no amount of donation buys a tier", got.Plan)
			}
		})
	}
}

// Stripe redelivers. A replayed donation must not produce a second document:
// the delivery key is unique and the event id is checked, and for an anonymous
// gift both of those hang off the platform tenant rather than a real one.
func TestARedeliveredAnonymousDonationIsAppliedOnce(t *testing.T) {
	st := newTenants()
	h, rc, _ := newHandler(t, st)

	body := donationEvent("evt_dup", "", "NL", 500, true)
	for i := range 2 {
		if w := deliver(t, h, body); w.Code != 200 {
			t.Fatalf("delivery %d: got %d: %s", i+1, w.Code, w.Body.String())
		}
	}
	if len(rc.rows) != 1 {
		t.Fatalf("receipts = %d, want 1: a redelivery must not bill twice", len(rc.rows))
	}
}

// A giver outside the EU is NOT parked. Place of supply is a question about a
// supply and there is none, so the country cannot decide anything -- the
// document issues whatever it says. Parking it would hold real income hostage
// to a decision nobody can make.
func TestADonationFromOutsideTheEUIsRecordedNotParked(t *testing.T) {
	st := newTenants()
	h, rc, _ := newHandler(t, st)

	if w := deliver(t, h, donationEvent("evt_us", "", "US", 2500, true)); w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if len(rc.rows) != 1 {
		t.Fatalf("receipts = %d, want 1", len(rc.rows))
	}
	if rc.rows[0].Country != "US" {
		t.Errorf("country = %q, want US frozen onto the receipt", rc.rows[0].Country)
	}
}

// A zero-amount session is nothing to document, marker or not.
func TestAZeroAmountSessionRecordsNothing(t *testing.T) {
	st := newTenants()
	h, rc, _ := newHandler(t, st)

	if w := deliver(t, h, donationEvent("evt_zero", "", "NL", 0, true)); w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if len(rc.rows) != 0 {
		t.Fatalf("receipts = %d, want 0", len(rc.rows))
	}
}

func failedInvoice(id, cus string, due int64, nextAttempt int64) string {
	return fmt.Sprintf(`{"id":%q,"type":%q,"created":%d,"data":{"object":{
		"id":"in_fail","customer":%q,"subscription":"sub_1","amount_due":%d,"amount_paid":0,
		"currency":"eur","customer_email":"payer@example.com","customer_name":"Payer",
		"attempt_count":1,"next_payment_attempt":%d}}}`,
		id, EventInvoiceFailed, fixedNow.Unix(), cus, due, nextAttempt)
}

// A failing card is the provider retrying, not a cancellation. Ending a plan
// over one attempt would take a fleet off the air because a card expired.
func TestAFailedPaymentDoesNotTouchThePlan(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1", Plan: plans.Crew, StripeCustomerID: "cus_1"})
	h, rc, _ := newHandler(t, st)

	if w := deliver(t, h, failedInvoice("evt_fail", "cus_1", 900, fixedNow.Add(72*time.Hour).Unix())); w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if got := st.get("t1"); got.Plan != plans.Crew {
		t.Errorf("plan = %q, want crew kept: a retry is not a cancellation", got.Plan)
	}
	// And no receipt: nothing was paid, so nothing is owed a document.
	if len(rc.rows) != 0 {
		t.Errorf("receipts = %d, want 0: a failed payment is not a sale", len(rc.rows))
	}
}

// The event has to be handled at all. Before this, six events were routed and
// none of them was a failure, so a declined or Radar-blocked renewal left no
// metric, no audit entry and no word to the customer.
func TestAFailedPaymentIsAcknowledged(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1", Plan: plans.Crew, StripeCustomerID: "cus_1"})
	h, _, _ := newHandler(t, st)

	w := deliver(t, h, failedInvoice("evt_ack", "cus_1", 900, 0))
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
}

// A zero-amount invoice failing is a proration or a trial and means nothing.
func TestAZeroAmountFailureIsIgnored(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1", Plan: plans.Crew, StripeCustomerID: "cus_1"})
	h, _, _ := newHandler(t, st)

	if w := deliver(t, h, failedInvoice("evt_zero_fail", "cus_1", 0, 0)); w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if got := st.get("t1"); got.Plan != plans.Crew {
		t.Errorf("plan = %q", got.Plan)
	}
}

// A failure for a customer nobody knows is still money-adjacent and still has
// to be visible, the same as any other unattributable event.
func TestAFailedPaymentWithNoTenantIsRecordedForAPerson(t *testing.T) {
	st := newTenants()
	h, _, _ := newHandler(t, st)

	if w := deliver(t, h, failedInvoice("evt_orphan_fail", "cus_unknown", 900, 0)); w.Code != 200 {
		t.Fatalf("got %d: %s (an unattributable event is acknowledged, not retried)", w.Code, w.Body.String())
	}
}

// The first alert written against this counter fired within minutes of being
// deployed -- for a probe whose tenant row had been deleted while Stripe was
// still sending trailing subscription events. No money was involved in any of
// them. Paging a human for that is how a pager stops being believed, so the
// counter distinguishes money from lifecycle and only the first is tier-1.
func TestOnlyRealMoneyCountsAsAnUnattributedPayment(t *testing.T) {
	h, _, _ := newHandler(t, &fakeTenants{})

	before := counterValue(t, "payment")
	beforeLifecycle := counterValue(t, "lifecycle")

	// A subscription event naming a tenant nobody knows: no money moved.
	h.recordUnattributed(context.Background(),
		Event{ID: "evt_a", Type: EventSubscriptionDeleted}, 0, "", "", "subscription event with no tenant")
	if got := counterValue(t, "payment"); got != before {
		t.Errorf("a zero-amount lifecycle event moved the payment counter (%v -> %v); "+
			"it would page somebody for a deleted test tenant", before, got)
	}
	if got := counterValue(t, "lifecycle"); got != beforeLifecycle+1 {
		t.Errorf("the lifecycle counter did not move; the event would be invisible")
	}

	// Money that belongs to nobody: somebody has been charged and will get no
	// receipt and no VAT document.
	h.recordUnattributed(context.Background(),
		Event{ID: "evt_b", Type: EventInvoicePaid}, 900, "eur", "someone@example.test", "invoice paid with no tenant")
	if got := counterValue(t, "payment"); got != before+1 {
		t.Errorf("real unattributed money did not move the payment counter (%v -> %v)", before, got)
	}
}

func counterValue(t *testing.T, kind string) float64 {
	t.Helper()
	var m dto.Metric
	c, err := metrics.PaymentsUnattributedTotal.GetMetricWithLabelValues(kind)
	if err != nil {
		t.Fatalf("reading the counter: %v", err)
	}
	if err := c.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("writing the counter: %v", err)
	}
	return m.GetCounter().GetValue()
}
