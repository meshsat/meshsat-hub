package stripe

import (
	"fmt"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/billing"
	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
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
