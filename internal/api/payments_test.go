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
	"github.com/meshsat/meshsat-hub/internal/kofi"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

func paymentsRouter(t *testing.T, s store.Store, ms *mockStore) (http.Handler, *audit.Service) {
	t.Helper()
	a := audit.New(s)
	h := NewPaymentsHandler(a, ms)
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
func TestUnmatchedPaymentsAreListedForAnOperator(t *testing.T) {
	s := newAuditStore(t)
	r, a := paymentsRouter(t, s, &mockStore{})
	ctx := t.Context()

	detail := `{"transaction_id":"txn-77","tier":"Crew","amount":"9.00","currency":"EUR","payer_email":"someone@example.com","message":"no code here","reason":"no claim code, payer address, or remembered payer matched a tenant"}`
	if err := a.Log(ctx, store.DefaultTenantID, kofi.UnmatchedAction, "kofi_webhook", detail, ""); err != nil {
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
	// Everything needed to attribute it by hand.
	for _, k := range []string{"transaction_id", "amount", "currency", "payer_email", "reason"} {
		if p[k] == nil || p[k] == "" {
			t.Errorf("payment record is missing %q: %v", k, p)
		}
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
