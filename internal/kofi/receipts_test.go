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
