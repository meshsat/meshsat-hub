package stripe

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// The first real subscription payment, EUR 9.00 on 2026-09-11, took the money
// and produced no VAT document. It was recorded as "invoice paid with no
// tenant" while the subscription itself was granted correctly, which is the
// shape of the bug: the tenant WAS on the invoice event, in a field this code
// did not read.
//
// Stripe API 2026-08-26 ("dahlia") moved three things off the top level of an
// invoice:
//
//	subscription        -> parent.subscription_details.subscription
//	lines[].price       -> lines[].pricing.price_details.price
//	charge              -> removed
//
// and put the subscription's metadata -- the tenant_id this Hub sets -- at
// parent.subscription_details.metadata. onInvoicePaid passed nil metadata and
// could only fall back to the customer binding, which on a FIRST subscription
// does not exist yet: Stripe sends invoice.paid a second BEFORE
// checkout.session.completed, and the latter is what writes the binding. So
// every first-time subscriber lost their first document, silently, while
// everything else about the payment looked right.
//
// testdata/invoice_paid_dahlia.json is the real event off the live account,
// with ids and personal details replaced. It is a fixture rather than a
// hand-written literal on purpose: the bug was a wrong belief about the wire
// format, and only the real payload can contradict that.

func dahliaInvoice(t *testing.T) invoice {
	t.Helper()
	raw, err := os.ReadFile("testdata/invoice_paid_dahlia.json")
	if err != nil {
		t.Fatalf("reading the captured event: %v", err)
	}
	var ev Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("decoding the event: %v", err)
	}
	var inv invoice
	if err := json.Unmarshal(ev.Data.Object, &inv); err != nil {
		t.Fatalf("decoding the invoice: %v", err)
	}
	return inv
}

func TestTheTenantIsReadableFromARealInvoiceEvent(t *testing.T) {
	inv := dahliaInvoice(t)

	if got := inv.metadata()["tenant_id"]; got != "default" {
		t.Fatalf("tenant_id read from the invoice = %q, want \"default\"; this is the "+
			"field whose absence cost the first payment its document", got)
	}
	if got := inv.subscriptionID(); got != "sub_EXAMPLE" {
		t.Errorf("subscriptionID() = %q, want the id under parent.subscription_details", got)
	}
	if got := inv.priceID(); got != "price_crew_EXAMPLE" {
		t.Errorf("priceID() = %q, want the id under lines[].pricing.price_details", got)
	}
	// The fields that did still work, so a fix cannot quietly break them.
	if inv.AmountPaid != 900 || strings.ToUpper(inv.Currency) != "EUR" {
		t.Errorf("amount/currency = %d/%s", inv.AmountPaid, inv.Currency)
	}
	if inv.Customer == "" || inv.CustomerEmail == "" {
		t.Errorf("customer = %q, email = %q", inv.Customer, inv.CustomerEmail)
	}
	if inv.country() != "NL" {
		t.Errorf("country() = %q, want NL", inv.country())
	}
}

// The whole point: a receipt is produced even though nothing has bound this
// customer to a tenant yet. This is the ordering Stripe actually uses on a
// first subscription, so it is the ordering that has to work.
func TestAFirstInvoiceIsDocumentedBeforeTheCustomerIsBound(t *testing.T) {
	raw, err := os.ReadFile("testdata/invoice_paid_dahlia.json")
	if err != nil {
		t.Fatal(err)
	}
	// A tenant that exists but has NO stripe_customer_id -- exactly the state
	// checkout.session.completed has not yet written.
	st := newTenants(store.Tenant{ID: "default", Plan: plans.Free, BillingCountry: "NL"})
	h, rc, _ := newHandler(t, st)
	h.SetPrices(map[string]string{"price_crew_EXAMPLE": plans.Crew})

	if w := deliver(t, h, string(raw)); w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	if len(rc.rows) != 1 {
		t.Fatalf("wrote %d receipts, want 1; the payment would have no VAT document", len(rc.rows))
	}
	r := rc.rows[0]
	if r.TenantID != "default" {
		t.Errorf("receipt went to tenant %q", r.TenantID)
	}
	if r.AmountCents != 900 || r.Currency != "EUR" {
		t.Errorf("receipt is for %d %s", r.AmountCents, r.Currency)
	}
	if r.Plan != plans.Crew {
		t.Errorf("receipt plan = %q, want crew; the price is only readable from the new field", r.Plan)
	}
	if r.Country != "NL" {
		t.Errorf("receipt country = %q; the VAT gate needs this", r.Country)
	}
}

// The old shape has to keep working: Stripe replays events at the version they
// were created with, so an event from before the upgrade can still arrive.
func TestThePreDahliaInvoiceShapeStillResolves(t *testing.T) {
	body := `{"id":"evt_old","type":"invoice.paid","created":1789095909,"data":{"object":{
		"id":"in_old","customer":"cus_bound","subscription":"sub_old",
		"amount_paid":900,"currency":"eur","customer_email":"c@example.org",
		"customer_address":{"country":"NL"},
		"lines":{"data":[{"price":{"id":"price_crew"}}]}}}}`

	st := newTenants(store.Tenant{ID: "t-a", Plan: plans.Free, StripeCustomerID: "cus_bound", BillingCountry: "NL"})
	h, rc, _ := newHandler(t, st)

	if w := deliver(t, h, body); w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(rc.rows) != 1 {
		t.Fatalf("wrote %d receipts, want 1", len(rc.rows))
	}
	if rc.rows[0].Plan != plans.Crew {
		t.Errorf("plan = %q, want crew from the legacy lines[].price", rc.rows[0].Plan)
	}
}

// Money that genuinely belongs to nobody must still be parked rather than
// guessed at. The fix widens what can be resolved; it must not invent a tenant.
func TestAnInvoiceWithNoTenantAnywhereIsStillParked(t *testing.T) {
	body := `{"id":"evt_orphan","type":"invoice.paid","created":1789095909,"data":{"object":{
		"id":"in_orphan","customer":"cus_unknown",
		"amount_paid":900,"currency":"eur","customer_email":"nobody@example.org",
		"parent":{"subscription_details":{"subscription":"sub_x","metadata":{}}},
		"lines":{"data":[{"pricing":{"price_details":{"price":"price_crew"}}}]}}}}`

	st := newTenants(store.Tenant{ID: "t-a", Plan: plans.Free, StripeCustomerID: "cus_other"})
	h, rc, _ := newHandler(t, st)

	if w := deliver(t, h, body); w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if len(rc.rows) != 0 {
		t.Fatalf("wrote %d receipts for money belonging to nobody", len(rc.rows))
	}
}
