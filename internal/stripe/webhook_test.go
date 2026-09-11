package stripe

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
)

var fixedNow = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

// --- fakes ----------------------------------------------------------------

type fakeTenants struct {
	mu      sync.Mutex
	tenants []store.Tenant
	events  map[string]bool
	updates int
}

func newTenants(ts ...store.Tenant) *fakeTenants {
	return &fakeTenants{tenants: ts, events: map[string]bool{}}
}

func (f *fakeTenants) GetTenant(_ context.Context, id string) (*store.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.tenants {
		if f.tenants[i].ID == id {
			t := f.tenants[i]
			return &t, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeTenants) TenantByStripeCustomer(_ context.Context, cus string) (*store.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.tenants {
		if f.tenants[i].StripeCustomerID != "" && f.tenants[i].StripeCustomerID == cus {
			t := f.tenants[i]
			return &t, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeTenants) UpdateTenant(_ context.Context, t *store.Tenant) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.tenants {
		if f.tenants[i].ID == t.ID {
			f.tenants[i] = *t
			f.updates++
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeTenants) ApplyStripeEvent(_ context.Context, eventID, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.events[eventID] {
		return false, nil
	}
	f.events[eventID] = true
	return true, nil
}

func (f *fakeTenants) get(id string) store.Tenant {
	t, _ := f.GetTenant(context.Background(), id)
	if t == nil {
		return store.Tenant{}
	}
	return *t
}

type fakeReceipts struct {
	mu   sync.Mutex
	rows []store.Receipt
}

func (f *fakeReceipts) CreateReceipt(_ context.Context, r *store.Receipt) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if f.rows[i].DeliveryKey == r.DeliveryKey {
			return false, nil
		}
	}
	f.rows = append(f.rows, *r)
	return true, nil
}

// Both added with the payment_ref column: the webhook records which provider
// payment settled a receipt so a refund can find the document to reverse.
func (f *fakeReceipts) SetReceiptPaymentRef(_ context.Context, deliveryKey, paymentRef string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if f.rows[i].DeliveryKey == deliveryKey {
			f.rows[i].PaymentRef = paymentRef
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeReceipts) GetReceiptByPaymentRef(_ context.Context, ref string) (*store.Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if ref != "" && f.rows[i].PaymentRef == ref {
			out := f.rows[i]
			return &out, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeReceipts) GetReceiptByKey(_ context.Context, k string) (*store.Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if f.rows[i].DeliveryKey == k {
			r := f.rows[i]
			return &r, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeReceipts) ListDueReceipts(context.Context, time.Time, int) ([]store.Receipt, error) {
	return nil, nil
}
func (f *fakeReceipts) SetReceiptInvoice(context.Context, string, string) error { return nil }
func (f *fakeReceipts) MarkReceiptIssued(context.Context, string, string, string, time.Time) error {
	return nil
}
func (f *fakeReceipts) MarkReceiptAttempt(context.Context, string, string, time.Time) error {
	return nil
}
func (f *fakeReceipts) BlockReceipt(context.Context, string, string) error { return nil }
func (f *fakeReceipts) ClaimReceipt(context.Context, string, time.Time) (bool, error) {
	return true, nil
}
func (f *fakeReceipts) ReleaseReceipt(context.Context, string) error { return nil }

type fakeRefunds struct {
	mu   sync.Mutex
	rows []store.Refund
}

func (f *fakeRefunds) CreateRefund(_ context.Context, r *store.Refund) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if f.rows[i].ReceiptID == r.ReceiptID {
			return false, nil // receipt_id is UNIQUE: a payment is refunded once
		}
	}
	f.rows = append(f.rows, *r)
	return true, nil
}

// --- harness --------------------------------------------------------------

func newHandler(t *testing.T, st *fakeTenants) (*Handler, *fakeReceipts, *fakeRefunds) {
	t.Helper()
	rc, rf := &fakeReceipts{}, &fakeRefunds{}
	h := NewHandler(st, testSecret)
	h.SetReceipts(rc)
	h.SetRefunds(rf)
	h.now = func() time.Time { return fixedNow }
	h.SetPrices(map[string]string{"price_crew": plans.Crew, "price_fleet": plans.Fleet})
	return h, rc, rf
}

// deliver posts a signed event, as Stripe would.
func deliver(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/stripe/s3cr3t", strings.NewReader(body))
	req.Header.Set("Stripe-Signature", sign(t, []byte(body), fixedNow, testSecret))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func subEvent(id, typ, tenant, cus, price, sub, status string, periodEnd time.Time) string {
	return fmt.Sprintf(`{"id":%q,"type":%q,"created":%d,"data":{"object":{
		"id":%q,"customer":%q,"status":%q,"current_period_end":%d,
		"metadata":{"tenant_id":%q},
		"items":{"data":[{"price":{"id":%q}}]}}}}`,
		id, typ, fixedNow.Unix(), sub, cus, status, periodEnd.Unix(), tenant, price)
}

// --- tests ----------------------------------------------------------------

func TestASubscriptionGrantsThePlanUntilItsPeriodEnd(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1", Plan: plans.Free})
	h, _, _ := newHandler(t, st)
	end := fixedNow.Add(30 * 24 * time.Hour)

	w := deliver(t, h, subEvent("evt_1", EventSubscriptionCreated, "t1", "cus_1", "price_crew", "sub_1", "active", end))
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	got := st.get("t1")
	if got.Plan != plans.Crew {
		t.Errorf("plan = %q, want crew", got.Plan)
	}
	if got.PlanExpiresAt == nil {
		t.Fatal("no expiry was set")
	}
	// The grace exists so a renewal webhook lost to an outage costs nobody
	// their ceiling while Stripe is still charging their card.
	if want := end.Add(grace); !got.PlanExpiresAt.Equal(want) {
		t.Errorf("expiry = %v, want the period end plus grace (%v)", got.PlanExpiresAt, want)
	}
	if got.StripeSubscriptionID != "sub_1" || got.StripeCustomerID != "cus_1" {
		t.Errorf("the subscription was not bound to the tenant: %+v", got)
	}
}

// The event Ko-fi could never send. It is why the lapse job is a safety net now
// rather than the mechanism.
func TestACancelledSubscriptionDropsThePlanImmediately(t *testing.T) {
	end := fixedNow.Add(30 * 24 * time.Hour)
	st := newTenants(store.Tenant{
		ID: "t1", Plan: plans.Fleet, PlanExpiresAt: &end,
		StripeCustomerID: "cus_1", StripeSubscriptionID: "sub_1",
	})
	h, _, _ := newHandler(t, st)

	w := deliver(t, h, subEvent("evt_x", EventSubscriptionDeleted, "t1", "cus_1", "price_fleet", "sub_1", "canceled", end))
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	got := st.get("t1")
	if got.Plan != plans.Free {
		t.Errorf("plan = %q, want free", got.Plan)
	}
	if got.PlanExpiresAt != nil {
		t.Errorf("a cancelled plan kept an expiry: %v", got.PlanExpiresAt)
	}
	if got.StripeSubscriptionID != "" {
		t.Error("the subscription id was not cleared, so an empty one no longer means no subscription")
	}
}

// A tier an operator set by hand has no expiry and never lapses. A cancelled
// subscription must not silently convert that into an ended plan.
func TestACancellationLeavesAnOperatorSetTierAlone(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1", Plan: plans.Custom, StripeCustomerID: "cus_1"})
	h, _, _ := newHandler(t, st)

	deliver(t, h, subEvent("evt_x", EventSubscriptionDeleted, "t1", "cus_1", "price_crew", "sub_1", "canceled", fixedNow))
	if got := st.get("t1"); got.Plan != plans.Custom {
		t.Errorf("an operator-set tier was ended by a cancellation: %q", got.Plan)
	}
}

// A card that failed is not a cancellation. Stripe retries for days and sends
// the deleted event if it gives up; taking a fleet's ceiling away mid-retry
// would be a worse answer than waiting.
func TestAPastDueSubscriptionKeepsItsPlan(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1", Plan: plans.Free})
	h, _, _ := newHandler(t, st)
	end := fixedNow.Add(24 * time.Hour)

	deliver(t, h, subEvent("evt_1", EventSubscriptionUpdated, "t1", "cus_1", "price_crew", "sub_1", "past_due", end))
	if got := st.get("t1"); got.Plan != plans.Crew {
		t.Errorf("plan = %q; a past_due card is a retry, not a cancellation", got.Plan)
	}
}

// Ko-fi's equivalent defaulted to Crew on a tier name it did not recognise,
// which is a guess about money. An unknown price changes nothing.
func TestAnUnknownPriceChangesNothing(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1", Plan: plans.Free})
	h, _, _ := newHandler(t, st)

	w := deliver(t, h, subEvent("evt_1", EventSubscriptionCreated, "t1", "cus_1", "price_MYSTERY", "sub_1", "active", fixedNow.Add(time.Hour)))
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	if got := st.get("t1"); got.Plan != plans.Free || got.PlanExpiresAt != nil {
		t.Errorf("an unmapped price bought something: %q until %v", got.Plan, got.PlanExpiresAt)
	}
}

// custom and beta are unlimited and operator-set. A price that resolved to
// either would sell an uncapped fleet for whatever that price happens to be.
func TestAPriceCannotBuyAnUnlimitedPlan(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1", Plan: plans.Free})
	h, _, _ := newHandler(t, st)
	h.SetPrices(map[string]string{
		"price_sneaky": plans.Custom,
		"price_beta":   plans.Beta,
		"price_crew":   plans.Crew,
	})
	for _, price := range []string{"price_sneaky", "price_beta"} {
		deliver(t, h, subEvent("evt_"+price, EventSubscriptionCreated, "t1", "cus_1", price, "sub_1", "active", fixedNow.Add(time.Hour)))
		if got := st.get("t1"); got.Plan != plans.Free {
			t.Fatalf("%s bought %q, which is unlimited and operator-set", price, got.Plan)
		}
	}
	// The sellable one still works, so the guard is not just refusing everything.
	deliver(t, h, subEvent("evt_ok", EventSubscriptionCreated, "t1", "cus_1", "price_crew", "sub_1", "active", fixedNow.Add(time.Hour)))
	if got := st.get("t1"); got.Plan != plans.Crew {
		t.Errorf("the sellable tier stopped working: %q", got.Plan)
	}
}

func TestAPaidInvoiceRecordsAReceipt(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1", Plan: plans.Crew, StripeCustomerID: "cus_1", BillingCountry: "NL"})
	h, rc, _ := newHandler(t, st)

	body := fmt.Sprintf(`{"id":"evt_inv","type":%q,"created":%d,"data":{"object":{
		"id":"in_123","customer":"cus_1","subscription":"sub_1","amount_paid":900,
		"currency":"eur","customer_email":"buyer@example.com","customer_name":"Buyer",
		"customer_address":{"country":"NL"},
		"lines":{"data":[{"price":{"id":"price_crew"}}]}}}}`, EventInvoicePaid, fixedNow.Unix())
	if w := deliver(t, h, body); w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if len(rc.rows) != 1 {
		t.Fatalf("receipts written = %d, want 1", len(rc.rows))
	}
	r := rc.rows[0]
	if r.AmountCents != 900 || r.Currency != "EUR" || r.Country != "NL" {
		t.Errorf("receipt = %+v", r)
	}
	if r.DeliveryKey != "stripe:inv:in_123" {
		t.Errorf("delivery key = %q; the refund path recomputes this from the charge", r.DeliveryKey)
	}
	if r.TenantID != "t1" {
		t.Errorf("receipt tenant = %q", r.TenantID)
	}
}

// Stripe redelivers until it gets a 2xx. Two replicas serving the same retry
// must not both apply it.
func TestARedeliveredEventAppliesOnce(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1", Plan: plans.Crew, StripeCustomerID: "cus_1"})
	h, rc, _ := newHandler(t, st)
	body := fmt.Sprintf(`{"id":"evt_dup","type":%q,"created":%d,"data":{"object":{
		"id":"in_1","customer":"cus_1","amount_paid":900,"currency":"eur",
		"customer_email":"b@e.example","lines":{"data":[{"price":{"id":"price_crew"}}]}}}}`,
		EventInvoicePaid, fixedNow.Unix())

	for i := 0; i < 4; i++ {
		if w := deliver(t, h, body); w.Code != 200 {
			t.Fatalf("delivery %d got %d", i, w.Code)
		}
	}
	if len(rc.rows) != 1 {
		t.Fatalf("a redelivered event wrote %d receipts", len(rc.rows))
	}
}

// The whole point of the migration: a refund made in Stripe produces a credit
// note with no operator involved.
func TestARefundInStripeRecordsARefundRowByItself(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1", Plan: plans.Crew, StripeCustomerID: "cus_1", BillingCountry: "NL"})
	h, rc, rf := newHandler(t, st)
	rc.rows = append(rc.rows, store.Receipt{
		ID: "rcpt-1", TenantID: "t1", DeliveryKey: "stripe:inv:in_123",
		AmountCents: 900, Currency: "EUR", Country: "NL", Status: store.ReceiptIssued,
	})

	body := fmt.Sprintf(`{"id":"evt_ref","type":%q,"created":%d,"data":{"object":{
		"id":"ch_1","invoice":"in_123","customer":"cus_1","amount":900,
		"amount_refunded":900,"currency":"eur","refunded":true}}}`, EventChargeRefunded, fixedNow.Unix())
	if w := deliver(t, h, body); w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if len(rf.rows) != 1 {
		t.Fatalf("refunds written = %d, want 1 — this is the operator step the migration removes", len(rf.rows))
	}
	r := rf.rows[0]
	if r.ReceiptID != "rcpt-1" || r.AmountCents != 900 {
		t.Errorf("refund = %+v", r)
	}
	// Frozen from the receipt: a customer who moves must not change the VAT on
	// a document already issued.
	if r.Country != "NL" {
		t.Errorf("refund country = %q, want the receipt's", r.Country)
	}
	if r.RequestedBy != "stripe" {
		t.Errorf("requested_by = %q; the trail should say this was not a person", r.RequestedBy)
	}
}

// A refund whose payment this Hub never documented must be loud, not silent.
func TestARefundWithNoReceiptIsRecordedForAPerson(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1", StripeCustomerID: "cus_1"})
	h, _, rf := newHandler(t, st)

	body := fmt.Sprintf(`{"id":"evt_ref","type":%q,"created":%d,"data":{"object":{
		"id":"ch_1","invoice":"in_UNKNOWN","customer":"cus_1","amount":900,
		"amount_refunded":900,"currency":"eur"}}}`, EventChargeRefunded, fixedNow.Unix())
	w := deliver(t, h, body)
	if w.Code != 200 {
		t.Fatalf("got %d; Stripe must not be made to retry this for three days", w.Code)
	}
	if len(rf.rows) != 0 {
		t.Error("a refund row was invented for a payment with no document")
	}
}

// A donation buys a document and no tier.
func TestADonationGetsAReceiptAndNoPlan(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1", Plan: plans.Free})
	h, rc, _ := newHandler(t, st)

	body := fmt.Sprintf(`{"id":"evt_cs","type":%q,"created":%d,"data":{"object":{
		"id":"cs_1","customer":"cus_1","payment_intent":"pi_1","mode":"payment",
		"amount_total":1000,"currency":"eur","metadata":{"tenant_id":"t1"},
		"customer_details":{"email":"donor@example.com","name":"Donor","address":{"country":"DE"}}}}}`,
		EventCheckoutCompleted, fixedNow.Unix())
	if w := deliver(t, h, body); w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if got := st.get("t1"); got.Plan != plans.Free {
		t.Errorf("a donation bought a tier: %q", got.Plan)
	}
	if len(rc.rows) != 1 {
		t.Fatalf("receipts = %d, want 1", len(rc.rows))
	}
	if rc.rows[0].DeliveryKey != "stripe:pi:pi_1" {
		t.Errorf("delivery key = %q; the refund path recomputes this from the payment intent",
			rc.rows[0].DeliveryKey)
	}
	// The field Ko-fi never sent. Without it every donation from a stranger
	// parked in the blocked list instead of issuing.
	if got := st.get("t1"); got.BillingCountry != "DE" {
		t.Errorf("billing country = %q, want the one collected at checkout", got.BillingCountry)
	}
}

// A country already on the tenant is evidence gathered at enrolment. A card's
// billing address is weaker and must not overwrite it.
func TestCheckoutNeverOverwritesAKnownCountry(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1", Plan: plans.Free, BillingCountry: "NL"})
	h, _, _ := newHandler(t, st)

	body := fmt.Sprintf(`{"id":"evt_cs","type":%q,"created":%d,"data":{"object":{
		"id":"cs_1","customer":"cus_1","mode":"subscription","metadata":{"tenant_id":"t1"},
		"customer_details":{"email":"b@e.example","address":{"country":"FR"}}}}}`,
		EventCheckoutCompleted, fixedNow.Unix())
	deliver(t, h, body)
	if got := st.get("t1"); got.BillingCountry != "NL" {
		t.Errorf("billing country = %q; a card address overwrote declared evidence", got.BillingCountry)
	}
}

// A payment this Hub cannot place is recorded for a person, not retried: no
// amount of redelivery will make a tenant appear.
func TestAnEventWithNoTenantIsAcknowledgedNotRetried(t *testing.T) {
	st := newTenants()
	h, _, _ := newHandler(t, st)

	w := deliver(t, h, subEvent("evt_1", EventSubscriptionCreated, "t-nobody", "cus_nobody", "price_crew", "sub_1", "active", fixedNow))
	if w.Code != 200 {
		t.Fatalf("got %d, want 200 so Stripe stops retrying", w.Code)
	}
}

func TestAnUnsignedDeliveryIsRefused(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1"})
	h, _, _ := newHandler(t, st)

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/stripe/s3cr3t", strings.NewReader(`{"id":"evt_1"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an unsigned delivery got %d, want 400", w.Code)
	}
	// And it must not say which part failed: that turns the endpoint into an
	// oracle for somebody probing it.
	if strings.Contains(strings.ToLower(w.Body.String()), "timestamp") {
		t.Error("the refusal explains what was wrong with the signature")
	}
}

func TestAnEventTypeThisHubIgnoresIsStillAcknowledged(t *testing.T) {
	st := newTenants(store.Tenant{ID: "t1"})
	h, _, _ := newHandler(t, st)

	body := fmt.Sprintf(`{"id":"evt_1","type":"payment_method.attached","created":%d,"data":{"object":{}}}`, fixedNow.Unix())
	if w := deliver(t, h, body); w.Code != 200 {
		t.Fatalf("got %d; an unhandled type would be retried for three days", w.Code)
	}
}

func TestAnUnconfiguredHandlerRefusesEverything(t *testing.T) {
	h := NewHandler(newTenants(), "")
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/stripe/x", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", w.Code)
	}
}
