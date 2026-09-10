package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/store"
)

func refundRouter(s *mockStore) http.Handler {
	h := NewRefundsHandler(nil, s)
	r := chi.NewRouter()
	r.Post("/api/admin/receipts/{id}/refund", h.RecordRefund)
	r.Get("/api/admin/refunds", h.ListRefunds)
	r.Post("/api/admin/refunds/{id}/requeue", h.RequeueRefund)
	r.Delete("/api/admin/refunds/{id}", h.DeleteRefund)
	return r
}

func paid() *store.Receipt {
	return &store.Receipt{
		ID: "rcpt-1", TenantID: "t1", AmountCents: 900, Currency: "EUR",
		Country: "NL", Status: store.ReceiptIssued, InvoiceRef: "inv-1",
		PaidAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}
}

func post(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestRecordRefundDefaultsToTheWholePayment(t *testing.T) {
	s := &mockStore{receipt: paid()}
	w := post(t, refundRouter(s), "/api/admin/receipts/rcpt-1/refund",
		map[string]any{"reason": "14-day withdrawal"})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if s.createdRefund == nil {
		t.Fatal("no refund was recorded")
	}
	if s.createdRefund.AmountCents != 900 {
		t.Fatalf("amount = %d, want the whole payment", s.createdRefund.AmountCents)
	}
	// The currency and country are copied from the payment and frozen: the
	// credit note must reverse the VAT the invoice charged, whatever the
	// customer's address says today.
	if s.createdRefund.Currency != "EUR" || s.createdRefund.Country != "NL" {
		t.Fatalf("the refund did not inherit the payment's terms: %+v", s.createdRefund)
	}
	if s.createdRefund.ReceiptID != "rcpt-1" || s.createdRefund.TenantID != "t1" {
		t.Fatalf("the refund is not bound to the payment: %+v", s.createdRefund)
	}
}

// TestARefundLargerThanThePaymentIsRefused: a credit note for more than was
// ever charged reclaims VAT that was never declared.
func TestARefundLargerThanThePaymentIsRefused(t *testing.T) {
	s := &mockStore{receipt: paid()}
	w := post(t, refundRouter(s), "/api/admin/receipts/rcpt-1/refund",
		map[string]any{"amount_cents": 1200})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
	}
	if s.createdRefund != nil {
		t.Fatal("a refund of more than was paid was recorded")
	}
	if !strings.Contains(w.Body.String(), "9.00") {
		t.Fatalf("the message does not say what was actually paid: %s", w.Body)
	}
}

func TestARefundOfNothingIsRefused(t *testing.T) {
	s := &mockStore{receipt: paid()}
	w := post(t, refundRouter(s), "/api/admin/receipts/rcpt-1/refund",
		map[string]any{"amount_cents": -100})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestOneRefundPerPayment. A second one would credit money that only went back
// once; the operator withdraws the first if the figure was wrong.
func TestOneRefundPerPayment(t *testing.T) {
	s := &mockStore{receipt: paid(), refundExists: true}
	w := post(t, refundRouter(s), "/api/admin/receipts/rcpt-1/refund", map[string]any{})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body)
	}
}

func TestRefundOfAnUnknownPaymentIs404(t *testing.T) {
	s := &mockStore{} // no receipt
	w := post(t, refundRouter(s), "/api/admin/receipts/nope/refund", map[string]any{})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// TestARefundCannotPredateThePayment: the refund date is what dates the credit
// note, and a credit note dated before the invoice it corrects is a wrong
// document.
func TestARefundCannotPredateThePayment(t *testing.T) {
	s := &mockStore{receipt: paid()}
	w := post(t, refundRouter(s), "/api/admin/receipts/rcpt-1/refund",
		map[string]any{"refunded_at": "2026-08-01T00:00:00Z"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
	}
}

func TestRefundedAtMustBeATime(t *testing.T) {
	s := &mockStore{receipt: paid()}
	w := post(t, refundRouter(s), "/api/admin/receipts/rcpt-1/refund",
		map[string]any{"refunded_at": "yesterday"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestListRefundsRejectsAnUnknownState(t *testing.T) {
	s := &mockStore{}
	req := httptest.NewRequest(http.MethodGet, "/api/admin/refunds?status=nonsense", nil)
	w := httptest.NewRecorder()
	refundRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestListRefundsDefaultsToPending(t *testing.T) {
	s := &mockStore{refunds: []store.Refund{{ID: "rf-1", Status: store.RefundPending}}}
	req := httptest.NewRequest(http.MethodGet, "/api/admin/refunds", nil)
	w := httptest.NewRecorder()
	refundRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var out []store.Refund
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out) != 1 {
		t.Fatalf("body = %s err = %v", w.Body, err)
	}
}

func TestRequeueOfAnIssuedRefundIs404(t *testing.T) {
	s := &mockStore{refundReqErr: store.ErrNotFound}
	w := post(t, refundRouter(s), "/api/admin/refunds/rf-1/requeue", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body)
	}
}

// TestWithdrawingACreditedRefundIs404. Once the document exists, deleting the
// row would hide the credit note rather than unmake it.
func TestWithdrawingACreditedRefundIs404(t *testing.T) {
	s := &mockStore{refundDelErr: store.ErrNotFound}
	req := httptest.NewRequest(http.MethodDelete, "/api/admin/refunds/rf-1", nil)
	w := httptest.NewRecorder()
	refundRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestWithdrawingAnUncreditedRefundSucceeds(t *testing.T) {
	s := &mockStore{}
	req := httptest.NewRequest(http.MethodDelete, "/api/admin/refunds/rf-1", nil)
	w := httptest.NewRecorder()
	refundRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", w.Code, w.Body)
	}
	if len(s.refundDeleted) != 1 {
		t.Fatalf("nothing was withdrawn: %v", s.refundDeleted)
	}
}

func TestMoneyRendersMinorUnitsExactly(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{{900, "9.00"}, {1, "0.01"}, {0, "0.00"}, {123456, "1234.56"}, {-900, "-9.00"}} {
		if got := money(tc.in); got != tc.want {
			t.Fatalf("money(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
