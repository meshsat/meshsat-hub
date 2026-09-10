package kofi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/invoiceninja"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// memReceipts is the outbox in memory, with the one property that matters
// preserved: the delivery key is unique and a second insert reports false.
type memReceipts struct {
	mu     sync.Mutex
	rows   []*store.Receipt
	n      int
	fail   bool
	leases map[string]time.Time
	// heldBy lets a test pretend another drainer already holds a row.
	held map[string]bool
}

func (m *memReceipts) CreateReceipt(_ context.Context, r *store.Receipt) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return false, errors.New("store is down")
	}
	if r.DeliveryKey == "" {
		return false, errors.New("delivery key is required")
	}
	for _, e := range m.rows {
		if e.DeliveryKey == r.DeliveryKey {
			return false, nil
		}
	}
	m.n++
	cp := *r
	if cp.ID == "" {
		cp.ID = "rcp-" + strconv.Itoa(m.n)
		r.ID = cp.ID
	}
	if cp.Status == "" {
		cp.Status = store.ReceiptPending
	}
	m.rows = append(m.rows, &cp)
	return true, nil
}

func (m *memReceipts) GetReceiptByKey(_ context.Context, k string) (*store.Receipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.rows {
		if e.DeliveryKey == k {
			cp := *e
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *memReceipts) ListDueReceipts(_ context.Context, now time.Time, limit int) ([]store.Receipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.Receipt
	for _, e := range m.rows {
		if e.Status == store.ReceiptPending && !e.NextAttemptAt.After(now) {
			out = append(out, *e)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memReceipts) find(id string) *store.Receipt {
	for _, e := range m.rows {
		if e.ID == id {
			return e
		}
	}
	return nil
}

func (m *memReceipts) SetReceiptInvoice(_ context.Context, id, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.find(id); e != nil {
		e.InvoiceRef = ref
	}
	return nil
}

func (m *memReceipts) MarkReceiptIssued(_ context.Context, id, num, ref string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.find(id); e != nil {
		e.Status, e.InvoiceNumber, e.InvoiceRef, e.LastError = store.ReceiptIssued, num, ref, ""
		t := at
		e.IssuedAt = &t
	}
	return nil
}

func (m *memReceipts) MarkReceiptAttempt(_ context.Context, id, msg string, next time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.find(id); e != nil {
		e.Attempts++
		e.LastError, e.NextAttemptAt = msg, next
	}
	return nil
}

// ClaimReceipt models the conditional UPDATE: one winner, and a lease that
// expires on its own.
func (m *memReceipts) ClaimReceipt(_ context.Context, id string, until time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.held[id] {
		return false, nil
	}
	if m.leases == nil {
		m.leases = map[string]time.Time{}
	}
	if t, ok := m.leases[id]; ok && t.After(time.Now()) {
		return false, nil
	}
	m.leases[id] = until
	return true, nil
}

func (m *memReceipts) ReleaseReceipt(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.leases, id)
	return nil
}

func (m *memReceipts) BlockReceipt(_ context.Context, id, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.find(id); e != nil {
		e.Status, e.LastError = store.ReceiptBlocked, reason
	}
	return nil
}

// fakeIssuer stands in for the billing system.
type fakeIssuer struct {
	mu       sync.Mutex
	calls    []invoiceninja.Request
	err      error
	failOnce error
	// createdInvoice is handed to OnInvoiceCreated before err is returned, so
	// a test can reproduce "the invoice exists but the payment failed".
	createdInvoice string
}

func (f *fakeIssuer) IssueReceipt(_ context.Context, req invoiceninja.Request) (*invoiceninja.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if f.createdInvoice != "" && req.ExistingInvoiceID == "" && req.OnInvoiceCreated != nil {
		if err := req.OnInvoiceCreated(f.createdInvoice); err != nil {
			return nil, err
		}
	}
	if f.failOnce != nil {
		err := f.failOnce
		f.failOnce = nil
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	return &invoiceninja.Result{InvoiceID: "inv-1", InvoiceNumber: "MSH2026-0001"}, nil
}

func paidPayload(msgID, txn string) Payload {
	return Payload{
		VerificationToken: token, IsSubscriptionPayment: true,
		TierName: "Crew", Email: "someone-else@example.com",
		Message: "here you go! AB2K9XYZ", MessageID: msgID, KofiTransactionID: txn,
		Amount: "9.00", Currency: "EUR", Timestamp: "2026-09-10T09:00:00Z",
	}
}

func handlerWithReceipts(st *memTenants, rc *memReceipts) *Handler {
	h := newHandler(st)
	h.SetReceipts(rc)
	return h
}

func TestPaymentRecordsOneReceipt(t *testing.T) {
	st, rc := newStore(), &memReceipts{}
	h := handlerWithReceipts(st, rc)

	if rr := post(t, h, paidPayload("msg-1", "txn-1")); rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	if len(rc.rows) != 1 {
		t.Fatalf("recorded %d receipts, want 1", len(rc.rows))
	}
	r := rc.rows[0]
	if r.TenantID != "t-a" || r.AmountCents != 900 || r.Currency != "EUR" {
		t.Fatalf("receipt = %+v", r)
	}
	if r.DeliveryKey != "msg-1" {
		t.Errorf("delivery key = %q, want the Ko-fi message id", r.DeliveryKey)
	}
	// The receipt goes to the ACCOUNT owner's address, not to whatever address
	// the card happened to be under.
	if r.Email != "alice@example.com" {
		t.Errorf("receipt email = %q, want the tenant owner's account address", r.Email)
	}
	if !r.PaidAt.Equal(time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("paid_at = %v, want Ko-fi's own timestamp", r.PaidAt)
	}
}

// A retried delivery must not produce a second document.
func TestReplayedDeliveryRecordsNoSecondReceipt(t *testing.T) {
	st, rc := newStore(), &memReceipts{}
	h := handlerWithReceipts(st, rc)

	post(t, h, paidPayload("msg-1", "txn-1"))
	post(t, h, paidPayload("msg-1", "txn-1"))
	if len(rc.rows) != 1 {
		t.Fatalf("recorded %d receipts for one payment", len(rc.rows))
	}
}

// Ko-fi's message_id is a third party's field and can be missing. An absent id
// must not become an empty key that collides with every other empty key.
func TestMissingMessageIDStillDedupes(t *testing.T) {
	st, rc := newStore(), &memReceipts{}
	h := handlerWithReceipts(st, rc)

	post(t, h, paidPayload("", "txn-7"))
	post(t, h, paidPayload("", "txn-7"))
	if len(rc.rows) != 1 {
		t.Fatalf("recorded %d receipts, want 1", len(rc.rows))
	}
	if rc.rows[0].DeliveryKey != "txn:txn-7" {
		t.Errorf("delivery key = %q, want the transaction fallback", rc.rows[0].DeliveryKey)
	}

	// Neither id at all: still a stable key, and two different payments still
	// get two rows.
	p := paidPayload("", "")
	post(t, h, p)
	post(t, h, p)
	q := paidPayload("", "")
	q.Amount = "29.00"
	post(t, h, q)
	if len(rc.rows) != 3 {
		t.Fatalf("rows = %d, want 3 (txn fallback, digest, a different digest)", len(rc.rows))
	}
	if rc.rows[1].DeliveryKey == rc.rows[2].DeliveryKey {
		t.Error("two different payments collapsed onto one delivery key")
	}
}

// The receipt records the payment, not the grant. If granting the plan fails,
// the money still arrived and the customer is still owed a document.
func TestReceiptRecordedEvenWhenTheGrantFails(t *testing.T) {
	st, rc := newStore(), &memReceipts{}
	st.failNext = true
	h := handlerWithReceipts(st, rc)

	rr := post(t, h, paidPayload("msg-1", "txn-1"))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500 so Ko-fi retries the grant", rr.Code)
	}
	if len(rc.rows) != 1 {
		t.Fatalf("the payment was not recorded when the grant failed: %d rows", len(rc.rows))
	}
}

// A handler with no billing system configured still grants plans.
func TestNoReceiptStoreStillGrantsThePlan(t *testing.T) {
	st := newStore()
	h := newHandler(st)
	if rr := post(t, h, paidPayload("msg-1", "txn-1")); rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	got, _ := st.GetTenant(context.Background(), "t-a")
	if got.PlanExpiresAt == nil {
		t.Fatal("no plan was granted")
	}
}

func TestDrainerIssuesTheReceipt(t *testing.T) {
	rc := &memReceipts{}
	iss := &fakeIssuer{}
	_, _ = rc.CreateReceipt(context.Background(), &store.Receipt{
		TenantID: "t-a", Country: "NL", DeliveryKey: "msg-1", Email: "alice@example.com", Name: "Alpha",
		AmountCents: 900, Currency: "EUR", Plan: "crew", TierName: "Crew Membership",
		PaidAt: time.Now().UTC(),
	})
	j := NewReceiptJob(rc, iss, nil)
	j.Once(context.Background())

	if len(iss.calls) != 1 {
		t.Fatalf("issuer called %d times", len(iss.calls))
	}
	req := iss.calls[0]
	if req.AmountCents != 900 || req.Currency != "EUR" || req.Email != "alice@example.com" {
		t.Errorf("request = %+v", req)
	}
	if req.CustomerRef != "t-a" {
		t.Errorf("customer ref = %q, want the tenant id so the two systems can be reconciled", req.CustomerRef)
	}
	r := rc.rows[0]
	if r.Status != store.ReceiptIssued || r.InvoiceNumber != "MSH2026-0001" {
		t.Fatalf("receipt not marked issued: %+v", r)
	}

	// Already issued: the next pass must not send it again.
	j.Once(context.Background())
	if len(iss.calls) != 1 {
		t.Fatalf("an issued receipt was sent again (%d calls)", len(iss.calls))
	}
}

// A billing system that is down must not lose the document.
func TestRetryableFailureKeepsTheReceiptPending(t *testing.T) {
	rc := &memReceipts{}
	iss := &fakeIssuer{err: &invoiceninja.Error{Status: 502, Op: "create invoice", Body: "bad gateway"}}
	_, _ = rc.CreateReceipt(context.Background(), &store.Receipt{
		TenantID: "t-a", Country: "NL", DeliveryKey: "msg-1", Email: "alice@example.com",
		AmountCents: 900, Currency: "EUR", PaidAt: time.Now().UTC(),
	})
	now := time.Now().UTC()
	j := NewReceiptJob(rc, iss, nil)
	j.now = func() time.Time { return now }
	j.Once(context.Background())

	r := rc.rows[0]
	if r.Status != store.ReceiptPending {
		t.Fatalf("status = %q, want pending -- money that arrived is never dropped", r.Status)
	}
	if r.Attempts != 1 || !r.NextAttemptAt.After(now) {
		t.Fatalf("no backoff recorded: attempts=%d next=%v", r.Attempts, r.NextAttemptAt)
	}

	// It recovers when the billing system does.
	iss.err = nil
	j.now = func() time.Time { return r.NextAttemptAt.Add(time.Second) }
	j.Once(context.Background())
	if rc.rows[0].Status != store.ReceiptIssued {
		t.Fatalf("status = %q after recovery", rc.rows[0].Status)
	}
}

// A request the billing system will always reject is parked for a person
// rather than retried forever.
func TestNonRetryableFailureIsParked(t *testing.T) {
	rc := &memReceipts{}
	iss := &fakeIssuer{err: &invoiceninja.Error{Status: 422, Op: "create invoice", Body: "validation"}}
	_, _ = rc.CreateReceipt(context.Background(), &store.Receipt{
		TenantID: "t-a", Country: "NL", DeliveryKey: "msg-1", Email: "alice@example.com",
		AmountCents: 900, Currency: "EUR", PaidAt: time.Now().UTC(),
	})
	j := NewReceiptJob(rc, iss, nil)
	j.Once(context.Background())
	if rc.rows[0].Status != store.ReceiptBlocked {
		t.Fatalf("status = %q, want blocked", rc.rows[0].Status)
	}
	j.Once(context.Background())
	if len(iss.calls) != 1 {
		t.Errorf("a blocked receipt is still being retried (%d calls)", len(iss.calls))
	}
}

func TestWrongCurrencyIsParkedNotConverted(t *testing.T) {
	rc := &memReceipts{}
	iss := &fakeIssuer{err: invoiceninja.ErrWrongCurrency}
	_, _ = rc.CreateReceipt(context.Background(), &store.Receipt{
		TenantID: "t-a", Country: "NL", DeliveryKey: "msg-1", Email: "alice@example.com",
		AmountCents: 900, Currency: "USD", PaidAt: time.Now().UTC(),
	})
	j := NewReceiptJob(rc, iss, nil)
	j.Once(context.Background())
	if rc.rows[0].Status != store.ReceiptBlocked {
		t.Fatalf("status = %q, want blocked", rc.rows[0].Status)
	}
}

func TestNoAddressIsParked(t *testing.T) {
	rc := &memReceipts{}
	iss := &fakeIssuer{}
	_, _ = rc.CreateReceipt(context.Background(), &store.Receipt{
		TenantID: "t-a", Country: "NL", DeliveryKey: "msg-1", AmountCents: 900, Currency: "EUR",
		PaidAt: time.Now().UTC(),
	})
	j := NewReceiptJob(rc, iss, nil)
	j.Once(context.Background())
	if rc.rows[0].Status != store.ReceiptBlocked {
		t.Fatalf("status = %q, want blocked", rc.rows[0].Status)
	}
	if len(iss.calls) != 0 {
		t.Error("a receipt with nowhere to go was still sent to the billing system")
	}
}

// The invoice id has to be persisted the moment the invoice exists. An invoice
// that has been sent has taken a number out of a gapless series; a retry that
// created a second one would leave the first permanently unpaid.
func TestInvoiceIDIsPersistedBeforeTheRetry(t *testing.T) {
	rc := &memReceipts{}
	iss := &fakeIssuer{createdInvoice: "inv-99", failOnce: &invoiceninja.Error{Status: 503, Op: "record payment"}}
	_, _ = rc.CreateReceipt(context.Background(), &store.Receipt{
		TenantID: "t-a", Country: "NL", DeliveryKey: "msg-1", Email: "alice@example.com",
		AmountCents: 900, Currency: "EUR", PaidAt: time.Now().UTC(),
	})
	now := time.Now().UTC()
	j := NewReceiptJob(rc, iss, nil)
	j.now = func() time.Time { return now }
	j.Once(context.Background())

	if rc.rows[0].InvoiceRef != "inv-99" {
		t.Fatalf("invoice ref = %q; a retry would create a second numbered invoice", rc.rows[0].InvoiceRef)
	}

	j.now = func() time.Time { return rc.rows[0].NextAttemptAt.Add(time.Second) }
	j.Once(context.Background())
	if len(iss.calls) != 2 {
		t.Fatalf("calls = %d", len(iss.calls))
	}
	if iss.calls[1].ExistingInvoiceID != "inv-99" {
		t.Errorf("the retry did not resume onto the existing invoice: %+v", iss.calls[1])
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	if got := backoff(0); got != time.Minute {
		t.Errorf("backoff(0) = %v", got)
	}
	if got := backoff(3); got != 8*time.Minute {
		t.Errorf("backoff(3) = %v", got)
	}
	if got := backoff(50); got != maxBackoff {
		t.Errorf("backoff(50) = %v, want the cap %v", got, maxBackoff)
	}
}

// A store that cannot record the payment must not turn the webhook into a 500:
// Ko-fi would replay a payment that has already been applied.
func TestReceiptStoreFailureStillAcknowledgesThePayment(t *testing.T) {
	st, rc := newStore(), &memReceipts{fail: true}
	h := handlerWithReceipts(st, rc)
	if rr := post(t, h, paidPayload("msg-1", "txn-1")); rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	got, _ := st.GetTenant(context.Background(), "t-a")
	if got.PlanExpiresAt == nil {
		t.Fatal("the plan was not granted")
	}
}

// An amount we cannot read is parked, not dropped. It used to return early with
// only a log line: the plan was granted, no receipt row existed, so the outbox
// had nothing to retry and the blocked list showed nothing. Money taken, no
// document, and no way to find out (MESHSAT-1016).
func TestUnparseableAmountIsParkedForAPerson(t *testing.T) {
	st, rc := newStore(), &memReceipts{}
	h := handlerWithReceipts(st, rc)
	p := paidPayload("msg-1", "txn-1")
	p.Amount = "nine euros"
	if rr := post(t, h, p); rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	if len(rc.rows) != 1 {
		t.Fatalf("an unreadable amount left %d receipt rows, want 1 parked", len(rc.rows))
	}
	row := rc.rows[0]
	if row.Status != store.ReceiptBlocked {
		t.Errorf("status = %q, want %q", row.Status, store.ReceiptBlocked)
	}
	for _, want := range []string{"nine euros", "could not be read"} {
		if !strings.Contains(row.LastError, want) {
			t.Errorf("the parked reason %q does not mention %q", row.LastError, want)
		}
	}
	if row.TransactionID != "txn-1" {
		t.Errorf("the parked row does not carry the transaction reference an operator needs: %q", row.TransactionID)
	}
	// The plan is still granted: somebody paid.
	got, _ := st.GetTenant(context.Background(), "t-a")
	if got.PlanExpiresAt == nil {
		t.Fatal("the plan was not granted")
	}
}

func TestDeliveryKeyPrefersTheMessageID(t *testing.T) {
	if k := deliveryKey(Payload{MessageID: " m1 ", KofiTransactionID: "t1"}); k != "m1" {
		t.Errorf("key = %q, want the trimmed message id", k)
	}
	if k := deliveryKey(Payload{KofiTransactionID: "t1"}); k != "txn:t1" {
		t.Errorf("key = %q", k)
	}
	k := deliveryKey(Payload{Email: "a@b.c", Amount: "9.00"})
	if len(k) < 10 || k[:7] != "digest:" {
		t.Errorf("key = %q, want a digest fallback", k)
	}
}

// The drainer must not touch the billing system for a row another drainer
// holds. The store's lease is the guard; this pins that the job actually asks
// for it, and asks BEFORE issuing rather than after (MESHSAT-998).
func TestDrainerSkipsAReceiptAnotherDrainerHolds(t *testing.T) {
	rc := &memReceipts{held: map[string]bool{}}
	now := time.Now().UTC()
	r := &store.Receipt{
		ID: "rcp-held", TenantID: "default", DeliveryKey: "k-held", Email: "a@b.c", Country: "NL",
		AmountCents: 900, Currency: "EUR", Plan: "crew", Status: store.ReceiptPending,
		PaidAt: now, NextAttemptAt: now.Add(-time.Minute),
	}
	if _, err := rc.CreateReceipt(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	rc.held["rcp-held"] = true // somebody else has it

	iss := &countingIssuer{}
	j := NewReceiptJob(rc, iss, nil)
	j.Once(context.Background())

	if iss.calls != 0 {
		t.Errorf("the billing system was called %d times for a receipt another drainer holds", iss.calls)
	}

	// Once released, the same pass picks it up.
	rc.held["rcp-held"] = false
	j.Once(context.Background())
	if iss.calls != 1 {
		t.Errorf("billing calls after release = %d, want 1", iss.calls)
	}
}

type countingIssuer struct{ calls int }

func (c *countingIssuer) IssueReceipt(_ context.Context, _ invoiceninja.Request) (*invoiceninja.Result, error) {
	c.calls++
	return &invoiceninja.Result{InvoiceID: "inv-1", InvoiceNumber: "MSH2026-0001"}, nil
}

// Where the buyer is decides whether Dutch VAT applies at all, and the question
// has to be asked BEFORE the billing system is touched: a sent invoice has
// taken a number out of a gapless series, so a document at the wrong rate
// cannot simply be deleted afterwards. Every buyer used to be invoiced as Dutch
// at 21% because the country was never asked for (MESHSAT-1016).
func TestAReceiptIsParkedUnlessTheCountryAllowsDutchVAT(t *testing.T) {
	for _, tc := range []struct {
		name    string
		country string
		issued  bool
		reason  string
	}{
		{"the seller's own country", "NL", true, ""},
		{"an EU consumer under the threshold", "DE", true, ""},
		{"a buyer outside the EU", "US", false, "outside the EU"},
		{"a buyer we cannot place", "", false, "no country on file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc, iss := &memReceipts{}, &countingIssuer{}
			now := time.Now().UTC()
			if _, err := rc.CreateReceipt(context.Background(), &store.Receipt{
				ID: "rcp-" + tc.country, TenantID: "t-a", Country: tc.country,
				DeliveryKey: "k-" + tc.country, Email: "buyer@example.com", Name: "Buyer",
				AmountCents: 900, Currency: "EUR", Plan: "crew", TierName: "Crew",
				PaidAt: now, NextAttemptAt: now.Add(-time.Minute),
			}); err != nil {
				t.Fatal(err)
			}
			NewReceiptJob(rc, iss, nil).Once(context.Background())

			row := rc.rows[0]
			if tc.issued {
				if iss.calls != 1 {
					t.Fatalf("a %s buyer produced %d billing calls, want 1", tc.country, iss.calls)
				}
				return
			}
			if iss.calls != 0 {
				t.Errorf("the billing system was called %d times for a %q buyer; a wrong-rate invoice "+
					"takes a number out of a gapless series and cannot be undone", iss.calls, tc.country)
			}
			if row.Status != store.ReceiptBlocked {
				t.Errorf("status = %q, want %q", row.Status, store.ReceiptBlocked)
			}
			if !strings.Contains(row.LastError, tc.reason) {
				t.Errorf("parked reason %q does not mention %q", row.LastError, tc.reason)
			}
		})
	}
}

// A donation is not a subscription and not the free plan. Recording it as
// plans.Free described a EUR 5 tip as "MeshSat Hub Free, subscription, one
// month" on the customer's document.
func TestADonationIsNotDescribedAsASubscription(t *testing.T) {
	if got := productKey(DonationPlan); got != "MeshSat Hub support" {
		t.Errorf("product key = %q", got)
	}
	got := description(DonationPlan, "Crew")
	for _, wrong := range []string{"subscription", "one month", "Crew", "Free"} {
		if strings.Contains(got, wrong) {
			t.Errorf("a donation line says %q: %q", wrong, got)
		}
	}
	if got == "" {
		t.Error("a donation line is empty")
	}
}
