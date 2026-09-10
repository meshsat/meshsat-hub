package invoiceninja

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The fake below mirrors what the live company actually does, established by
// running the sequence by hand before any of this was written:
//
//	invoice created            -> status 1, no number, balance 0
//	mark_sent                  -> status 2, number, balance = amount
//	payment recorded           -> status 4, balance 0, paid_to_date = amount
//	credit created             -> status 1, no number
//	credit mark_sent           -> status 2, number, balance = amount
//	payments/refund            -> payment.refunded = amount AND the invoice
//	                              balance REOPENS to the amount
//	payment with credits+invoices -> both settle to 0
//
// The reopening in step 6 is the one nobody would guess, and the apply in step 7
// only makes sense because of it.

type fakeIN struct {
	t *testing.T

	invoiceStatus  int
	invoiceBalance float64
	invoiceAmount  float64
	paymentAmount  float64
	paymentRefund  float64

	creditCreated  bool
	creditStatus   int
	creditBalance  float64
	creditNumber   string
	creditsCreated int
	refunds        int
	applies        int

	failRefund int
}

func (f *fakeIN) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/invoices/"):
			resp := map[string]any{"data": map[string]any{
				"id": "inv-1", "number": "MSH2026-0001", "client_id": "cli-1",
				"status_id": f.invoiceStatus, "balance": f.invoiceBalance, "amount": f.invoiceAmount,
				"payments": []map[string]any{{
					"id": "pay-1", "amount": f.paymentAmount, "refunded": f.paymentRefund,
				}},
			}}
			_ = json.NewEncoder(w).Encode(resp)

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/credits":
			f.creditsCreated++
			f.creditCreated = true
			f.creditStatus = statusDraft
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"id": "credit-1", "number": "", "client_id": "cli-1",
				"status_id": statusDraft, "balance": 0}})

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/credits/"):
			if !f.creditCreated {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"not found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"id": "credit-1", "number": f.creditNumber, "client_id": "cli-1",
				"status_id": f.creditStatus, "balance": f.creditBalance}})

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/credits/bulk":
			if in["action"] != "mark_sent" {
				f.t.Fatalf("unexpected credit bulk action %v", in["action"])
			}
			f.creditStatus = statusSent
			f.creditNumber = "MSHCN2026-0001"
			f.creditBalance = f.invoiceAmount
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{
				"id": "credit-1", "number": f.creditNumber, "client_id": "cli-1",
				"status_id": statusSent, "balance": f.creditBalance}}})

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/payments/refund":
			f.refunds++
			if f.failRefund > 0 {
				f.failRefund--
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"message":"down"}`))
				return
			}
			f.paymentRefund = f.paymentAmount
			// Refunding reopens the invoice. Verified against the live system.
			f.invoiceBalance = f.invoiceAmount
			f.invoiceStatus = statusSent
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "pay-1"}})

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/payments":
			f.applies++
			if _, ok := in["credits"]; !ok {
				f.t.Fatal("the settling payment did not draw on the credit note")
			}
			if in["amount"] != float64(0) {
				f.t.Fatalf("the settling payment moved money (amount=%v)", in["amount"])
			}
			f.invoiceBalance, f.creditBalance = 0, 0
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "pay-2"}})

		default:
			f.t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
}

func newFakeClient(t *testing.T, f *fakeIN) (*Client, *httptest.Server) {
	srv := httptest.NewServer(f.handler())
	c := New(srv.URL, "token", 5*time.Second)
	return c, srv
}

func creditReq() CreditRequest {
	return CreditRequest{
		InvoiceID: "inv-1", AmountCents: 900, Currency: "EUR",
		ProductKey: "MeshSat Hub Crew", Description: "MeshSat Hub subscription, Crew, one month",
		RefundedAt: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC),
	}
}

func TestIssueCreditNoteRunsTheWholeSequence(t *testing.T) {
	f := &fakeIN{t: t, invoiceStatus: 4, invoiceBalance: 0, invoiceAmount: 9, paymentAmount: 9}
	c, srv := newFakeClient(t, f)
	defer srv.Close()

	var persisted string
	req := creditReq()
	req.OnCreditCreated = func(id string) error { persisted = id; return nil }

	res, err := c.IssueCreditNote(context.Background(), req)
	if err != nil {
		t.Fatalf("IssueCreditNote: %v", err)
	}
	if res.CreditNumber != "MSHCN2026-0001" || res.InvoiceNumber != "MSH2026-0001" {
		t.Fatalf("result = %+v", res)
	}
	if persisted != "credit-1" {
		t.Fatalf("the credit note id was not handed over before it was numbered (%q)", persisted)
	}
	if f.creditsCreated != 1 || f.refunds != 1 || f.applies != 1 {
		t.Fatalf("sequence ran wrong: created=%d refunded=%d applied=%d",
			f.creditsCreated, f.refunds, f.applies)
	}
	if f.invoiceBalance != 0 || f.creditBalance != 0 {
		t.Fatalf("the customer was left owing an invoice or holding an unused credit: inv=%v credit=%v",
			f.invoiceBalance, f.creditBalance)
	}
}

// TestARetryDoesNotProduceASecondCreditNote. A second credit note is a second
// legal document for money that only went back once.
func TestARetryDoesNotProduceASecondCreditNote(t *testing.T) {
	f := &fakeIN{t: t, invoiceStatus: 4, invoiceBalance: 0, invoiceAmount: 9,
		paymentAmount: 9, failRefund: 1}
	c, srv := newFakeClient(t, f)
	defer srv.Close()

	var persisted string
	req := creditReq()
	req.OnCreditCreated = func(id string) error { persisted = id; return nil }

	if _, err := c.IssueCreditNote(context.Background(), req); err == nil {
		t.Fatal("the first attempt should have failed at the refund step")
	}
	if f.creditsCreated != 1 {
		t.Fatalf("credits created on the first attempt: %d", f.creditsCreated)
	}

	// The caller persisted the id, so the retry resumes on the same document.
	req.ExistingCreditID = persisted
	res, err := c.IssueCreditNote(context.Background(), req)
	if err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if f.creditsCreated != 1 {
		t.Fatalf("the retry created a second credit note (%d in total)", f.creditsCreated)
	}
	if res.CreditNumber != "MSHCN2026-0001" {
		t.Fatalf("the retry produced a different document: %+v", res)
	}
}

// TestAnAlreadyRefundedPaymentIsNotRefundedAgain. The billing system refuses a
// double refund with a 422, but relying on being refused is not a design.
func TestAnAlreadyRefundedPaymentIsNotRefundedAgain(t *testing.T) {
	f := &fakeIN{t: t, invoiceStatus: statusSent, invoiceBalance: 9, invoiceAmount: 9,
		paymentAmount: 9, paymentRefund: 9, creditCreated: true,
		creditStatus: statusSent, creditNumber: "MSHCN2026-0001", creditBalance: 9}
	c, srv := newFakeClient(t, f)
	defer srv.Close()

	req := creditReq()
	req.ExistingCreditID = "credit-1"
	if _, err := c.IssueCreditNote(context.Background(), req); err != nil {
		t.Fatalf("IssueCreditNote: %v", err)
	}
	if f.refunds != 0 {
		t.Fatalf("the money was taken back twice (%d refunds)", f.refunds)
	}
	if f.applies != 1 {
		t.Fatalf("the credit was not applied to the reopened invoice (%d)", f.applies)
	}
}

// TestARefundLargerThanTheInvoiceIsRefused: a credit note for more than was
// ever charged reclaims VAT that was never declared.
func TestARefundLargerThanTheInvoiceIsRefused(t *testing.T) {
	f := &fakeIN{t: t, invoiceStatus: 4, invoiceBalance: 0, invoiceAmount: 9, paymentAmount: 9}
	c, srv := newFakeClient(t, f)
	defer srv.Close()

	req := creditReq()
	req.AmountCents = 1200
	_, err := c.IssueCreditNote(context.Background(), req)
	if err == nil {
		t.Fatal("a refund larger than the invoice was accepted")
	}
	if !strings.Contains(err.Error(), "more than invoice") {
		t.Fatalf("unhelpful error: %v", err)
	}
	if f.creditsCreated != 0 {
		t.Fatal("a document was created before the figure was checked")
	}
}

func TestAMissingInvoiceIsTerminal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"not found"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "token", 5*time.Second)

	_, err := c.IssueCreditNote(context.Background(), creditReq())
	if !errors.Is(err, ErrInvoiceGone) {
		t.Fatalf("want ErrInvoiceGone, got %v", err)
	}
}

func TestWrongCurrencyAndBadAmountNeverReachTheBillingSystem(t *testing.T) {
	f := &fakeIN{t: t, invoiceAmount: 9, paymentAmount: 9}
	c, srv := newFakeClient(t, f)
	defer srv.Close()

	req := creditReq()
	req.Currency = "USD"
	if _, err := c.IssueCreditNote(context.Background(), req); !errors.Is(err, ErrWrongCurrency) {
		t.Fatalf("want ErrWrongCurrency, got %v", err)
	}
	req = creditReq()
	req.AmountCents = 0
	if _, err := c.IssueCreditNote(context.Background(), req); !errors.Is(err, ErrBadAmount) {
		t.Fatalf("want ErrBadAmount, got %v", err)
	}
	if f.creditsCreated != 0 {
		t.Fatal("a document was created for a payment this code does not understand")
	}
}

// TestCreditPDFRefusesTheWebApp. This host answers an unknown path with its web
// application at 200, so a wrong URL comes back as HTML rather than an error --
// exactly how the inclusive-taxes check failed in production.
func TestCreditPDFRefusesTheWebApp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<!DOCTYPE html><html>the web app</html>"))
	}))
	defer srv.Close()
	c := New(srv.URL, "token", 5*time.Second)

	if _, err := c.CreditPDF(context.Background(), "credit-1"); err == nil {
		t.Fatal("HTML was accepted as a credit note PDF")
	} else if !strings.Contains(err.Error(), "not a PDF") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestCreditPDFReturnsTheDocument(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/download") {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte("%PDF-1.4\nbody"))
	}))
	defer srv.Close()
	c := New(srv.URL, "token", 5*time.Second)

	pdf, err := c.CreditPDF(context.Background(), "credit-1")
	if err != nil {
		t.Fatalf("CreditPDF: %v", err)
	}
	if !strings.HasPrefix(string(pdf), "%PDF") {
		t.Fatalf("not a PDF: %q", pdf[:8])
	}
}

// TestTheCreditLineNamesTheInvoiceItCorrects. A credit note that does not say
// what it corrects is not much of a correction.
func TestTheCreditLineNamesTheInvoiceItCorrects(t *testing.T) {
	var seen map[string]any
	var rawBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"id": "inv-1", "number": "MSH2026-0007", "client_id": "cli-1",
				"status_id": 4, "balance": 0, "amount": 9,
				"payments": []map[string]any{{"id": "pay-1", "amount": 9, "refunded": 9}}}})
		case r.URL.Path == "/api/v1/credits":
			body, _ := io.ReadAll(r.Body)
			rawBody = string(body)
			_ = json.Unmarshal(body, &seen)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"id": "credit-1", "number": "", "status_id": statusDraft, "balance": 0}})
		case r.URL.Path == "/api/v1/credits/bulk":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{
				"id": "credit-1", "number": "MSHCN2026-0001", "status_id": statusSent, "balance": 0}}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "x"}})
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "token", 5*time.Second)

	if _, err := c.IssueCreditNote(context.Background(), creditReq()); err != nil {
		t.Fatalf("IssueCreditNote: %v", err)
	}
	items, _ := seen["line_items"].([]any)
	if len(items) != 1 {
		t.Fatalf("line_items = %v", seen["line_items"])
	}
	line, _ := items[0].(map[string]any)
	notes, _ := line["notes"].(string)
	if !strings.Contains(notes, "MSH2026-0007") {
		t.Fatalf("the credit line does not name the invoice: %q", notes)
	}
	// GROSS, written as an exact decimal rather than a float: the company
	// derives the VAT out of it exactly as the invoice did, so the reversal
	// matches the charge. A rounding error in a VAT line is a wrong document.
	if !strings.Contains(rawBody, `"cost":9.00`) {
		t.Fatalf("the credit line does not carry the exact gross:\n%s", rawBody)
	}
	if line["tax_name1"] != "BTW 21" || line["tax_rate1"] != float64(21) {
		t.Fatalf("the credit note carries no VAT line: %v / %v", line["tax_name1"], line["tax_rate1"])
	}
}
