package invoiceninja

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeNinja is a stand-in for the billing system that records what it was
// asked to do, so the tests can assert on the sequence rather than the wire.
type fakeNinja struct {
	mu sync.Mutex
	// calls is the ordered list of "METHOD /path" this received.
	calls []string
	// bodies is the decoded JSON body of each call, same order.
	bodies []map[string]any

	customers map[string]string // email -> id
	invoice   struct {
		id       string
		number   string
		clientID string
		status   int
		balance  float64
	}
	failCreateInvoice int  // fail the next N create-invoice calls
	failMarkSent      int  // fail the next N mark_sent calls
	failPayment       int  // fail the next N payment calls
	invoiceMissing    bool // GET /invoices/{id} answers 404
}

func newFake() *fakeNinja {
	f := &fakeNinja{customers: map[string]string{}}
	f.invoice.id = "inv-1"
	f.invoice.status = statusDraft
	return f
}

func (f *fakeNinja) record(r *http.Request) map[string]any {
	body := map[string]any{}
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+strings.TrimPrefix(r.URL.Path, "/api/v1"))
	f.bodies = append(f.bodies, body)
	return body
}

func (f *fakeNinja) countCall(want string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == want {
			n++
		}
	}
	return n
}

func (f *fakeNinja) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v1/clients", func(w http.ResponseWriter, r *http.Request) {
		body := f.record(r)
		if r.Header.Get("X-API-TOKEN") != "test-token" {
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet {
			email := r.URL.Query().Get("email")
			if id, ok := f.customers[email]; ok {
				writeJSON(w, map[string]any{"data": []map[string]any{{"id": id}}})
				return
			}
			writeJSON(w, map[string]any{"data": []map[string]any{}})
			return
		}
		contacts, _ := body["contacts"].([]any)
		email := ""
		if len(contacts) > 0 {
			if m, ok := contacts[0].(map[string]any); ok {
				email, _ = m["email"].(string)
			}
		}
		f.mu.Lock()
		f.customers[email] = "cli-1"
		f.mu.Unlock()
		writeJSON(w, map[string]any{"data": map[string]any{"id": "cli-1"}})
	})

	mux.HandleFunc("/api/v1/invoices", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		f.mu.Lock()
		fail := f.failCreateInvoice > 0
		if fail {
			f.failCreateInvoice--
		}
		f.mu.Unlock()
		if fail {
			http.Error(w, `{"message":"boom"}`, http.StatusBadGateway)
			return
		}
		f.mu.Lock()
		f.invoice.clientID = "cli-1"
		inv := f.invoice
		f.mu.Unlock()
		writeJSON(w, map[string]any{"data": map[string]any{
			"id": inv.id, "number": inv.number, "client_id": inv.clientID,
			"status_id": inv.status, "balance": inv.balance,
		}})
	})

	mux.HandleFunc("/api/v1/invoices/bulk", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		f.mu.Lock()
		fail := f.failMarkSent > 0
		if fail {
			f.failMarkSent--
		} else {
			f.invoice.status = statusSent
			f.invoice.number = "MSH2026-0001"
			f.invoice.balance = 9
		}
		inv := f.invoice
		f.mu.Unlock()
		if fail {
			http.Error(w, `{"message":"boom"}`, http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]any{"data": []map[string]any{{
			"id": inv.id, "number": inv.number, "client_id": inv.clientID,
			"status_id": inv.status, "balance": inv.balance,
		}}})
	})

	mux.HandleFunc("/api/v1/invoices/inv-1", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		if f.invoiceMissing {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		f.mu.Lock()
		inv := f.invoice
		f.mu.Unlock()
		writeJSON(w, map[string]any{"data": map[string]any{
			"id": inv.id, "number": inv.number, "client_id": inv.clientID,
			// status_id and balance as strings: the v5 API does this on some
			// endpoints and a client that assumes numbers silently reads zero.
			"status_id": itoa(inv.status), "balance": ftoa(inv.balance),
		}})
	})

	mux.HandleFunc("/api/v1/payments", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		f.mu.Lock()
		fail := f.failPayment > 0
		if fail {
			f.failPayment--
		} else {
			f.invoice.balance = 0
		}
		f.mu.Unlock()
		if fail {
			http.Error(w, `{"message":"boom"}`, http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]any{"data": map[string]any{"id": "pay-1"}})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func itoa(i int) string { return strconv.Itoa(i) }
func ftoa(f float64) string {
	if f == 0 {
		return "0.00"
	}
	return amount(int64(f * 100)).String()
}

func testRequest() Request {
	return Request{
		CustomerRef: "tenant-abc",
		Name:        "A Customer",
		Email:       "customer@example.org",
		AmountCents: 900,
		Currency:    "EUR",
		ProductKey:  "MeshSat Hub Crew",
		Description: "MeshSat Hub subscription, Crew plan, one month",
		PaidAt:      time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
		Reference:   "kofi-txn-1",
	}
}

func TestIssueReceiptHappyPath(t *testing.T) {
	f := newFake()
	srv := f.server(t)
	c := New(srv.URL, "test-token", 5*time.Second)

	var recorded string
	req := testRequest()
	req.OnInvoiceCreated = func(id string) error { recorded = id; return nil }

	res, err := c.IssueReceipt(context.Background(), req)
	if err != nil {
		t.Fatalf("IssueReceipt: %v", err)
	}
	if res.InvoiceNumber != "MSH2026-0001" {
		t.Fatalf("invoice number = %q, want MSH2026-0001", res.InvoiceNumber)
	}
	if recorded != "inv-1" {
		t.Fatalf("OnInvoiceCreated got %q, want inv-1 -- a crash after this point would orphan the invoice", recorded)
	}

	// The line must carry the tax itself: the company default is a UI prefill
	// the API does not apply, so an invoice without it has no VAT at all.
	body := f.bodies[indexOf(t, f, "POST /invoices")]
	items, _ := body["line_items"].([]any)
	if len(items) != 1 {
		t.Fatalf("line_items = %v", body["line_items"])
	}
	item := items[0].(map[string]any)
	if item["tax_name1"] != "BTW 21" || item["tax_rate1"] != float64(21) {
		t.Errorf("line has no tax: %v", item)
	}
	if body["due_date"] != "2026-09-10" {
		t.Errorf("due_date = %v, want 2026-09-10 (payment terms are a UI prefill)", body["due_date"])
	}
	// GROSS: 9.00, not 9.00 plus VAT. json decodes it as a number, which is
	// what proves it was not sent as a string.
	if item["cost"] != float64(9) {
		t.Errorf("cost = %v (%T), want the gross 9", item["cost"], item["cost"])
	}

	pay := f.bodies[indexOf(t, f, "POST /payments")]
	if pay["email_receipt"] != "true" {
		t.Errorf("email_receipt = %v; the payment is what mails the customer", pay["email_receipt"])
	}
	if pay["transaction_reference"] != "kofi-txn-1" {
		t.Errorf("transaction_reference = %v", pay["transaction_reference"])
	}
}

func indexOf(t *testing.T, f *fakeNinja, call string) int {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.calls {
		if c == call {
			return i
		}
	}
	t.Fatalf("call %q never happened; calls were %v", call, f.calls)
	return -1
}

// TestRetryAfterPaymentFailureDoesNotDuplicate is the property the whole
// resumable design exists for: a receipt is money that was already taken, so
// the caller retries -- and a retry must produce neither a second invoice nor
// a second payment.
func TestRetryAfterPaymentFailureDoesNotDuplicate(t *testing.T) {
	f := newFake()
	f.failPayment = 1
	srv := f.server(t)
	c := New(srv.URL, "test-token", 5*time.Second)

	var invoiceID string
	req := testRequest()
	req.OnInvoiceCreated = func(id string) error { invoiceID = id; return nil }

	if _, err := c.IssueReceipt(context.Background(), req); err == nil {
		t.Fatal("first attempt should have failed at the payment")
	}
	if invoiceID == "" {
		t.Fatal("the invoice id was never handed back, so a retry cannot resume")
	}

	// The caller persisted the invoice id and tries again.
	req.ExistingInvoiceID = invoiceID
	res, err := c.IssueReceipt(context.Background(), req)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res.InvoiceNumber != "MSH2026-0001" {
		t.Fatalf("retry produced number %q", res.InvoiceNumber)
	}
	if n := f.countCall("POST /invoices"); n != 1 {
		t.Errorf("created %d invoices across the retry, want 1", n)
	}
	if n := f.countCall("POST /payments"); n != 2 {
		t.Errorf("payment attempts = %d, want 2 (one failed, one succeeded)", n)
	}
}

// TestRetryAfterSuccessPaysOnce covers the worse case: the payment succeeded
// but the caller never learned it did. The balance check has to stop a second
// payment against a settled invoice.
func TestRetryAfterSuccessPaysOnce(t *testing.T) {
	f := newFake()
	srv := f.server(t)
	c := New(srv.URL, "test-token", 5*time.Second)

	req := testRequest()
	res, err := c.IssueReceipt(context.Background(), req)
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	req.ExistingInvoiceID = res.InvoiceID
	res2, err := c.IssueReceipt(context.Background(), req)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res2.InvoiceNumber != res.InvoiceNumber {
		t.Errorf("retry returned a different number: %q vs %q", res2.InvoiceNumber, res.InvoiceNumber)
	}
	if n := f.countCall("POST /payments"); n != 1 {
		t.Errorf("paid %d times, want 1 -- a second payment invents a credit balance", n)
	}
	if n := f.countCall("POST /invoices"); n != 1 {
		t.Errorf("created %d invoices, want 1", n)
	}
}

// TestResumeWithDeletedInvoiceCreatesANewOne: the receipt is still owed even
// if somebody deleted the draft.
func TestResumeWithDeletedInvoiceCreatesANewOne(t *testing.T) {
	f := newFake()
	f.invoiceMissing = true
	srv := f.server(t)
	c := New(srv.URL, "test-token", 5*time.Second)

	req := testRequest()
	req.ExistingInvoiceID = "inv-1"
	if _, err := c.IssueReceipt(context.Background(), req); err != nil {
		t.Fatalf("IssueReceipt: %v", err)
	}
	if n := f.countCall("POST /invoices"); n != 1 {
		t.Errorf("did not recreate the deleted invoice (created %d)", n)
	}
}

func TestExistingCustomerIsReused(t *testing.T) {
	f := newFake()
	f.customers["customer@example.org"] = "cli-existing"
	srv := f.server(t)
	c := New(srv.URL, "test-token", 5*time.Second)

	if _, err := c.IssueReceipt(context.Background(), testRequest()); err != nil {
		t.Fatalf("IssueReceipt: %v", err)
	}
	if n := f.countCall("POST /clients"); n != 0 {
		t.Errorf("created %d duplicate customer records", n)
	}
}

func TestWrongCurrencyIsRefused(t *testing.T) {
	f := newFake()
	srv := f.server(t)
	c := New(srv.URL, "test-token", 5*time.Second)

	req := testRequest()
	req.Currency = "USD"
	_, err := c.IssueReceipt(context.Background(), req)
	if !errors.Is(err, ErrWrongCurrency) {
		t.Fatalf("err = %v, want ErrWrongCurrency -- converting on a tax document is worse than refusing", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("a refused currency still called the billing system: %v", f.calls)
	}
}

func TestErrorRetryability(t *testing.T) {
	cases := map[int]bool{500: true, 502: true, 429: true, 408: true, 400: false, 401: false, 404: false, 422: false}
	for status, want := range cases {
		e := &Error{Status: status}
		if got := e.Retryable(); got != want {
			t.Errorf("HTTP %d Retryable() = %v, want %v", status, got, want)
		}
	}
}

func TestUnconfiguredClientDoesNothing(t *testing.T) {
	c := New("", "", time.Second)
	if _, err := c.IssueReceipt(context.Background(), testRequest()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

// Every path here is relative: do() prepends /api/v1. Passing an absolute one
// produced /api/v1/api/v1/companies, and Invoice Ninja answers an unknown path
// with its web app at HTTP 200 rather than a 404 -- so the status check passed
// and the JSON decode failed on the first '<'. The guard fell back to a warning
// and stopped guarding anything, which is how it reached production
// (MESHSAT-1016).
func TestEveryRequestPathIsRelativeToTheAPIPrefix(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"settings":{"inclusive_taxes":true}}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok", 5*time.Second)
	on, err := c.VerifyInclusiveTaxes(context.Background())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !on {
		t.Error("inclusive taxes reported off when the company says on")
	}
	for _, p := range seen {
		if strings.Contains(p, "/api/v1/api/v1") {
			t.Errorf("path %q has the API prefix twice", p)
		}
		if !strings.HasPrefix(p, "/api/v1/") {
			t.Errorf("path %q does not sit under the API prefix", p)
		}
	}
}

// And the flag being OFF has to come back as false rather than an error, since
// that is the case the guard exists to shout about.
func TestInclusiveTaxesOffIsReportedNotSwallowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"settings":{"inclusive_taxes":false}}]}`))
	}))
	defer srv.Close()
	on, err := New(srv.URL, "tok", 5*time.Second).VerifyInclusiveTaxes(context.Background())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if on {
		t.Error("inclusive taxes reported on when the company says off")
	}
}

// TestVerifyInclusiveTaxesPicksTheTokensOwnCompany pins a live defect
// (MESHSAT-1019).
//
// GET /companies lists the ACCOUNT's companies, not the token's. This instance
// has two, and the first is the other business with inclusive taxes OFF -- so
// the guard shipped in MESHSAT-1016 has been reporting on a company these
// receipts never touch. Only per-company endpoints enforce the token's scope.
func TestVerifyInclusiveTaxesPicksTheTokensOwnCompany(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/companies":
			// data[0] is the other business, exactly as on the real instance.
			_, _ = w.Write([]byte(`{"data":[
				{"id":"other","settings":{"name":"Other Co","inclusive_taxes":false}},
				{"id":"ours","settings":{"name":"MeshSat Hub","inclusive_taxes":true}}]}`))
		case "/api/v1/companies/other":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"unauthorized"}`))
		case "/api/v1/companies/ours":
			_, _ = w.Write([]byte(`{"data":{"id":"ours","settings":{"inclusive_taxes":true}}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	on, err := New(srv.URL, "token", 5*time.Second).VerifyInclusiveTaxes(context.Background())
	if err != nil {
		t.Fatalf("VerifyInclusiveTaxes: %v", err)
	}
	if !on {
		t.Fatal("the guard read the other company on the instance, not the one this token issues into")
	}
}

// TestVerifyInclusiveTaxesRefusesToGuess. A false all-clear lets every receipt
// add VAT on top of a price the customer already paid.
func TestVerifyInclusiveTaxesRefusesToGuess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/companies" {
			_, _ = w.Write([]byte(`{"data":[
				{"id":"a","settings":{"inclusive_taxes":false}},
				{"id":"b","settings":{"inclusive_taxes":false}}]}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"unauthorized"}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "token", 5*time.Second).VerifyInclusiveTaxes(context.Background()); err == nil {
		t.Fatal("the guard guessed a company instead of saying it could not tell")
	}
}

// A single-company instance needs no probe at all.
func TestVerifyInclusiveTaxesOnASingleCompanyInstance(t *testing.T) {
	probes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/v1/companies" {
			probes++
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"only","settings":{"inclusive_taxes":true}}]}`))
	}))
	defer srv.Close()

	on, err := New(srv.URL, "token", 5*time.Second).VerifyInclusiveTaxes(context.Background())
	if err != nil || !on {
		t.Fatalf("on=%v err=%v", on, err)
	}
	if probes != 0 {
		t.Fatalf("a single-company instance was probed %d times", probes)
	}
}

// TestSenderIsPerCompanyOnlyForTheEmptyString pins a setting whose correct
// value is counter-intuitive and one click away from being wrong.
//
// NinjaMailerJob reaches setSelfHostMultiMailer() -- the only thing that
// applies a per-company sender on a self-hosted instance -- through the
// switch's `default` arm. 'default' is an explicit case that returns the
// instance-wide mailer early, and it is the class default for a new company.
// So the empty string is the only value that works, and the failure mode is a
// correctly signed email from the wrong business.
func TestSenderIsPerCompanyOnlyForTheEmptyString(t *testing.T) {
	for _, tc := range []struct {
		method string
		ok     bool
	}{
		{"", true},
		{"default", false}, // explicit case: returns the instance-wide mailer
		{"smtp", false},    // needs company smtp credentials, bails to 'default'
		{"gmail", false},
		{"office365", false},
	} {
		st := &CompanyState{EmailSendingMethod: tc.method}
		if st.SenderIsPerCompany() != tc.ok {
			t.Fatalf("SenderIsPerCompany(%q) = %v, want %v", tc.method, !tc.ok, tc.ok)
		}
	}
}

func TestInspectCompanyReadsTheSenderSetting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"only","settings":{
			"name":"MeshSat Hub","inclusive_taxes":true,
			"email_sending_method":"","reply_to_email":"billing@meshsat.net"}}]}`))
	}))
	defer srv.Close()

	st, err := New(srv.URL, "token", 5*time.Second).InspectCompany(context.Background())
	if err != nil {
		t.Fatalf("InspectCompany: %v", err)
	}
	if st.Name != "MeshSat Hub" || !st.InclusiveTaxes || !st.SenderIsPerCompany() ||
		st.ReplyToEmail != "billing@meshsat.net" {
		t.Fatalf("state = %+v", st)
	}
}

// Money outside the scope of VAT gets a line with no tax on it at all.
//
// This is not zero-rating within the system, it is money the system does not
// reach: a voluntary contribution with no counter-performance
// (internal/vat.ForDonation). The company has inclusive_taxes on, so leaving
// the rate in place would carve 21% out of a gift and print VAT on a document
// that owes none -- which the giver's own books could then reclaim.
func TestATaxExemptRequestCarriesNoTaxOnTheLine(t *testing.T) {
	f := newFake()
	srv := f.server(t)
	c := New(srv.URL, "test-token", 5*time.Second)

	req := testRequest()
	req.TaxExempt = true
	req.AmountCents = 500
	req.ProductKey = "MeshSat Hub support"
	if _, err := c.IssueReceipt(context.Background(), req); err != nil {
		t.Fatalf("IssueReceipt: %v", err)
	}

	body := f.bodies[indexOf(t, f, "POST /invoices")]
	items, _ := body["line_items"].([]any)
	if len(items) != 1 {
		t.Fatalf("line_items = %v", body["line_items"])
	}
	item := items[0].(map[string]any)
	if item["tax_name1"] != "" {
		t.Errorf("tax_name1 = %v, want empty: this money is outside the scope of BTW", item["tax_name1"])
	}
	if item["tax_rate1"] != float64(0) {
		t.Errorf("tax_rate1 = %v, want 0", item["tax_rate1"])
	}
	// The amount is still the whole gift. Nothing is derived out of it.
	if item["cost"] != float64(5) {
		t.Errorf("cost = %v, want the full 5.00", item["cost"])
	}
}

// Recording the payment is what mails the customer, so SuppressReceiptEmail has
// to reach that call and nothing else. It exists because this company has one
// payment template and it is written for a subscription: a donation sent under
// it tells the giver their subscription is active and that the document shows
// the VAT included in the price, when the document shows no tax at all.
func TestSuppressReceiptEmailReachesThePaymentCall(t *testing.T) {
	for _, tc := range []struct {
		name     string
		suppress bool
		want     string
	}{
		{"a subscription is mailed by the billing system", false, "true"},
		{"a donation is not, the Hub sends its own", true, "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			srv := f.server(t)
			c := New(srv.URL, "test-token", 5*time.Second)

			req := testRequest()
			req.SuppressReceiptEmail = tc.suppress
			if _, err := c.IssueReceipt(context.Background(), req); err != nil {
				t.Fatalf("IssueReceipt: %v", err)
			}

			pay := f.bodies[indexOf(t, f, "POST /payments")]
			if pay["email_receipt"] != tc.want {
				t.Errorf("email_receipt = %v, want %q", pay["email_receipt"], tc.want)
			}
			// It is a string, not a bool: this API rejects the bool.
			if _, ok := pay["email_receipt"].(string); !ok {
				t.Errorf("email_receipt is %T, want a string", pay["email_receipt"])
			}
		})
	}
}

// The invoice PDF travels with the donation receipt the Hub sends itself. Same
// checks as the credit note, because this host answers an unknown path with its
// web application at 200 rather than an error.
func TestInvoicePDFRefusesAnythingThatIsNotAPDF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/invoices/inv-1/download" {
			t.Errorf("fetched %q", r.URL.Path)
		}
		_, _ = w.Write([]byte("<!doctype html><html>the web app</html>"))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok", 5*time.Second)
	if _, err := c.InvoicePDF(context.Background(), "inv-1"); err == nil {
		t.Fatal("HTML was accepted as a PDF and would have been attached to a receipt")
	}
}

func TestInvoicePDFReturnsTheDocument(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("%PDF-1.4 receipt"))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok", 5*time.Second)
	pdf, err := c.InvoicePDF(context.Background(), "inv-1")
	if err != nil {
		t.Fatalf("InvoicePDF: %v", err)
	}
	if string(pdf) != "%PDF-1.4 receipt" {
		t.Errorf("got %q", pdf)
	}
}
