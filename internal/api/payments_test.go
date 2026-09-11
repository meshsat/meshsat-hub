package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/audit"
	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

func paymentsRouter(t *testing.T, s store.Store, ms *mockStore) (http.Handler, *audit.Service) {
	t.Helper()
	a := audit.New(s)
	h := NewPaymentsHandler(a, ms)
	// The rate production runs at. Without it the meter reports the gross,
	// which is the defect this pins.
	h.SetTaxRate(21)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			u := &hubauth.User{ID: "u1", Email: "admin@meshsat.net", PlatformAdmin: true}
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), hubauth.UserContextKey, u)))
		})
	})
	r.Get("/api/admin/payments/unmatched", h.ListUnmatched)
	r.Get("/api/admin/receipts/blocked", h.ListBlockedReceipts)
	r.Post("/api/admin/receipts/{id}/requeue", h.RequeueReceipt)
	r.Get("/api/admin/vat/threshold", h.VATThreshold)
	return r, a
}

func newAuditStore(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlite.New(":memory:", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// A payment that upgraded nobody has to be findable. Before this it was a
// slog.Warn and nothing else: nothing alerted on it, and an operator had no
// way to see that money had arrived (MESHSAT-1007).
//
// This seeds the action internal/stripe ACTUALLY writes. The version of this
// test that seeded the predecessor's action instead is why nobody noticed the
// endpoint had been returning [] since the Stripe migration.
func TestUnmatchedPaymentsAreListedForAnOperator(t *testing.T) {
	s := newAuditStore(t)
	r, a := paymentsRouter(t, s, &mockStore{})
	ctx := t.Context()

	detail := `{"provider":"stripe","event":"evt_77","type":"checkout.session.completed","amount_cents":900,"currency":"eur","payer_email":"someone@example.com","reason":"checkout completed with no tenant"}`
	if err := a.Log(ctx, store.DefaultTenantID, unattributedPaymentAction, "stripe_webhook", detail, ""); err != nil {
		t.Fatal(err)
	}
	// Noise on the same tenant must not appear in the listing.
	if err := a.Log(ctx, store.DefaultTenantID, "signup_approved", "admin", "email=x", ""); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/admin/payments/unmatched", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	var got []struct {
		RecordedAt string          `json:"recorded_at"`
		Payment    json.RawMessage `json:"payment"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if len(got) != 1 {
		t.Fatalf("got %d payments, want 1: %s", len(got), w.Body.String())
	}
	var p map[string]any
	if err := json.Unmarshal(got[0].Payment, &p); err != nil {
		t.Fatalf("payment detail is not JSON an operator can read: %v", err)
	}
	// Everything needed to attribute it by hand, in the shape
	// internal/stripe.recordUnattributed writes.
	for _, k := range []string{"event", "amount_cents", "currency", "payer_email", "reason"} {
		if p[k] == nil || p[k] == "" {
			t.Errorf("payment record is missing %q: %v", k, p)
		}
	}
}

// The audit log is an append-only hash chain, so entries the predecessor wrote
// cannot be rewritten under the current action name. They still have to be
// readable, or the migration silently hid money somebody already failed to
// place.
func TestAnUnattributedPaymentUnderTheOldActionIsStillListed(t *testing.T) {
	s := newAuditStore(t)
	r, a := paymentsRouter(t, s, &mockStore{})
	ctx := t.Context()

	legacy := `{"transaction_id":"txn-77","tier":"Crew","amount":"9.00","currency":"EUR","payer_email":"old@example.com","reason":"no claim code matched a tenant"}`
	if err := a.Log(ctx, store.DefaultTenantID, legacyUnattributedPaymentAction, "kofi_webhook", legacy, ""); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/admin/payments/unmatched", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var got []struct {
		Payment json.RawMessage `json:"payment"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if len(got) != 1 {
		t.Fatalf("got %d payments, want 1: the old action must stay readable: %s", len(got), w.Body.String())
	}
}

// Blocked was a one-way door: no API, no UI, no CLI reopened it, so a receipt
// parked for a person could only be fixed with hand-written SQL.
func TestABlockedReceiptCanBeListedAndRequeued(t *testing.T) {
	s := newAuditStore(t)
	ms := &mockStore{receipts: []store.Receipt{{
		ID: "rcp-1", TenantID: "t1", DeliveryKey: "k1", Email: "a@b.c",
		AmountCents: 900, Currency: "EUR", Status: store.ReceiptBlocked,
		LastError: "unexpected currency", CreatedAt: time.Now().UTC(),
	}}}
	r, _ := paymentsRouter(t, s, ms)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/receipts/blocked", nil))
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var list []store.Receipt
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list) != 1 {
		t.Fatalf("list: %v %s", err, w.Body.String())
	}
	if list[0].LastError == "" {
		t.Error("the listing does not say why it is blocked, which is the only useful part")
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/api/admin/receipts/rcp-1/requeue", nil))
	if w.Code != 200 {
		t.Fatalf("requeue: %d %s", w.Code, w.Body.String())
	}
	if len(ms.requeued) != 1 || ms.requeued[0] != "rcp-1" {
		t.Errorf("requeued %v, want [rcp-1]", ms.requeued)
	}

	// A receipt that is not blocked -- an issued one -- must not reopen, or the
	// drainer draws a second invoice number for a payment that has a document.
	ms.requeueErr = store.ErrNotFound
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/api/admin/receipts/rcp-issued/requeue", nil))
	if w.Code != 404 {
		t.Errorf("requeue of a non-blocked receipt: %d, want 404", w.Code)
	}
}

// The flat Dutch 21% is only correct while cross-border B2C sales stay under
// the Article 59c threshold, and nothing measured it. Domestic sales must not
// count: the threshold is about supplies to OTHER member states, and including
// our own would raise a false alarm on the busiest possible month and push the
// business toward an OSS registration it does not need (MESHSAT-1016).
func TestTheVATThresholdCountsOnlyCrossBorderEUSales(t *testing.T) {
	s := newAuditStore(t)
	ms := &mockStore{crossBorder: map[string]int64{
		"NL": 500000, // domestic: must be excluded
		"DE": 120000,
		"IE": 30000,
		"US": 900000, // not an EU sale at all
		"":   4200,   // unknown: cannot be counted as an EU supply
	}}
	r, _ := paymentsRouter(t, s, ms)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/vat/threshold", nil))
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		CrossBorderCents int64            `json:"cross_border_cents"`
		ThresholdCents   int64            `json:"threshold_cents"`
		PercentUsed      float64          `json:"percent_used"`
		ByCountry        map[string]int64 `json:"by_country"`
		Note             string           `json:"note"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	// DE 1200.00 + IE 300.00 gross, EXCLUDING VAT at 21%: 99173 + 24793.
	// The threshold is measured on supplies, not on the money that changed
	// hands, and this used to total the gross and read 21% high.
	if want := int64(99174 + 24793); got.CrossBorderCents != want {
		t.Errorf("cross-border total = %d, want %d (DE + IE, ex-VAT)", got.CrossBorderCents, want)
	}
	if got.CrossBorderCents >= 150000 {
		t.Error("the total still carries the VAT the customer paid")
	}
	if _, ok := got.ByCountry["NL"]; ok {
		t.Error("domestic Dutch sales are counted toward the cross-border threshold")
	}
	for _, c := range []string{"US", ""} {
		if _, ok := got.ByCountry[c]; ok {
			t.Errorf("%q is counted as an EU supply", c)
		}
	}
	if got.ThresholdCents != 1000000 {
		t.Errorf("threshold = %d cents, want 10 000 euro", got.ThresholdCents)
	}
	if got.PercentUsed < 12.3 || got.PercentUsed > 12.5 {
		t.Errorf("percent used = %v, want ~12.4 (ex-VAT)", got.PercentUsed)
	}
	if got.Note == "" {
		t.Error("the figure arrives with no explanation of what to do at the limit")
	}
}
