package refunds

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/invoiceninja"
	"github.com/meshsat/meshsat-hub/internal/kofi"
	"github.com/meshsat/meshsat-hub/internal/mail"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// --- fakes ---------------------------------------------------------------

type fakeStore struct {
	due      []store.Refund
	receipt  *store.Receipt
	tenant   *store.Tenant
	getRErr  error
	claimed  []string
	released []string

	issued        []string
	creditNumbers []string
	creditRefs    []string
	attempts      []string
	blocked       []string
	blockReasons  []string
	setCredits    []string
	blockedRcpts  []string
	updatedTenant *store.Tenant
	claimAll      bool
	issueErr      error

	getReceiptCalls    int
	onSecondGetReceipt func()
}

func (f *fakeStore) ListDueRefunds(context.Context, time.Time, int) ([]store.Refund, error) {
	return f.due, nil
}
func (f *fakeStore) GetReceipt(context.Context, string) (*store.Receipt, error) {
	if f.getRErr != nil {
		return nil, f.getRErr
	}
	f.getReceiptCalls++
	r := f.receipt
	if f.getReceiptCalls == 1 && f.onSecondGetReceipt != nil {
		f.onSecondGetReceipt() // the other drainer finishes between the reads
	}
	return r, nil
}
func (f *fakeStore) SetRefundCredit(_ context.Context, id, ref string) error {
	f.setCredits = append(f.setCredits, id+"="+ref)
	return nil
}
func (f *fakeStore) MarkRefundIssued(_ context.Context, id, number, ref string, _ time.Time) error {
	if f.issueErr != nil {
		return f.issueErr
	}
	f.issued = append(f.issued, id)
	f.creditNumbers = append(f.creditNumbers, number)
	f.creditRefs = append(f.creditRefs, ref)
	return nil
}
func (f *fakeStore) MarkRefundAttempt(_ context.Context, id, msg string, _ time.Time) error {
	f.attempts = append(f.attempts, id+": "+msg)
	return nil
}
func (f *fakeStore) BlockRefund(_ context.Context, id, reason string) error {
	f.blocked = append(f.blocked, id)
	f.blockReasons = append(f.blockReasons, reason)
	return nil
}
func (f *fakeStore) ClaimRefund(_ context.Context, id string, _ time.Time) (bool, error) {
	f.claimed = append(f.claimed, id)
	return !f.claimAll, nil
}
func (f *fakeStore) ReleaseRefund(_ context.Context, id string) error {
	f.released = append(f.released, id)
	return nil
}
func (f *fakeStore) BlockReceipt(_ context.Context, id, reason string) error {
	f.blockedRcpts = append(f.blockedRcpts, id+": "+reason)
	return nil
}
func (f *fakeStore) GetTenant(context.Context, string) (*store.Tenant, error) {
	return f.tenant, nil
}
func (f *fakeStore) UpdateTenant(_ context.Context, t *store.Tenant) error {
	f.updatedTenant = t
	return nil
}

type fakeIssuer struct {
	calls  int
	req    invoiceninja.CreditRequest
	result *invoiceninja.CreditResult
	err    error
	pdf    []byte
	pdfErr error
}

func (f *fakeIssuer) IssueCreditNote(_ context.Context, req invoiceninja.CreditRequest) (*invoiceninja.CreditResult, error) {
	f.calls++
	f.req = req
	if req.OnCreditCreated != nil {
		if err := req.OnCreditCreated("credit-new"); err != nil {
			return nil, err
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}
func (f *fakeIssuer) CreditPDF(context.Context, string) ([]byte, error) {
	if f.pdfErr != nil {
		return nil, f.pdfErr
	}
	if f.pdf == nil {
		return []byte("%PDF-1.4 fake"), nil
	}
	return f.pdf, nil
}

type fakeMailer struct {
	sent     []string
	attached []mail.Attachment
	bodies   []string
}

func (f *fakeMailer) Send(_ context.Context, to, subject, body string) error {
	f.sent = append(f.sent, to+" | "+subject)
	f.bodies = append(f.bodies, body)
	return nil
}
func (f *fakeMailer) SendWith(_ context.Context, to, subject, body string, a mail.Attachment) error {
	f.sent = append(f.sent, to+" | "+subject)
	f.bodies = append(f.bodies, body)
	f.attached = append(f.attached, a)
	return nil
}

func newJob(s *fakeStore, i Issuer) *Job {
	j := New(s, i, nil)
	j.now = func() time.Time { return time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC) }
	return j
}

func paidReceipt() *store.Receipt {
	return &store.Receipt{
		ID: "rcpt-1", TenantID: "t1", Email: "buyer@example.com", Name: "Buyer",
		AmountCents: 900, Currency: "EUR", Country: "NL", Plan: "crew", TierName: "Crew",
		Status: store.ReceiptIssued, InvoiceRef: "inv-1", InvoiceNumber: "MSH2026-0001",
	}
}

func pendingRefund(cents int64) store.Refund {
	return store.Refund{
		ID: "rf-1", TenantID: "t1", ReceiptID: "rcpt-1", AmountCents: cents,
		Currency: "EUR", Country: "NL", Status: store.RefundPending,
		RefundedAt: time.Date(2026, 9, 10, 11, 0, 0, 0, time.UTC),
	}
}

// --- tests ---------------------------------------------------------------

// TestRefundOfAnUnissuedPaymentCancelsTheReceipt is the interaction that makes
// this safe rather than just complete.
//
// A payment refunded while its receipt is still queued has no invoice to
// credit. Issuing a credit note against nothing would be a fabricated document;
// leaving the receipt pending is worse, because the OTHER drainer would then
// bill a customer for money that has already gone back. Cancelling the receipt
// is the whole of the correct action.
func TestRefundOfAnUnissuedPaymentCancelsTheReceipt(t *testing.T) {
	rcpt := paidReceipt()
	rcpt.InvoiceRef = "" // never issued
	rcpt.Status = store.ReceiptPending
	s := &fakeStore{
		due: []store.Refund{pendingRefund(900)}, receipt: rcpt,
		tenant: &store.Tenant{ID: "t1", Plan: "free"},
	}
	iss := &fakeIssuer{}
	m := &fakeMailer{}
	j := newJob(s, iss)
	j.SetMailer(m, "https://hub.meshsat.net")

	j.Once(context.Background())

	if iss.calls != 0 {
		t.Fatalf("a credit note was issued against an invoice that never existed (%d calls)", iss.calls)
	}
	if len(s.blockedRcpts) != 1 || !strings.Contains(s.blockedRcpts[0], "refunded before any invoice") {
		t.Fatalf("the receipt was not cancelled: %v", s.blockedRcpts)
	}
	if len(s.issued) != 1 || s.creditNumbers[0] != "" {
		t.Fatalf("the refund was not closed without a document: issued=%v numbers=%v", s.issued, s.creditNumbers)
	}
	if len(m.sent) != 1 || !strings.Contains(m.bodies[0], "no credit note to") {
		t.Fatalf("the customer was not told plainly: %v", m.sent)
	}
}

// TestFullRefundTakesBackThePaidPeriod: the refund gives back what a month cost,
// so it gives back the month. Only the expiry moves -- the hourly lapse job is
// the one piece of code that decides what a lapse means.
func TestFullRefundTakesBackThePaidPeriod(t *testing.T) {
	expires := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	s := &fakeStore{
		due: []store.Refund{pendingRefund(900)}, receipt: paidReceipt(),
		tenant: &store.Tenant{ID: "t1", Plan: "crew", PlanExpiresAt: &expires},
	}
	iss := &fakeIssuer{result: &invoiceninja.CreditResult{
		CreditID: "credit-1", CreditNumber: "MSHCN2026-0001", InvoiceNumber: "MSH2026-0001"}}
	j := newJob(s, iss)
	j.Once(context.Background())

	if s.updatedTenant == nil {
		t.Fatal("a full refund left the plan paid for a month nobody paid for")
	}
	want := expires.Add(-kofi.Period)
	if !s.updatedTenant.PlanExpiresAt.Equal(want) {
		t.Fatalf("expiry = %v, want %v (exactly the period the payment bought)",
			s.updatedTenant.PlanExpiresAt, want)
	}
	if s.updatedTenant.Plan != "crew" {
		t.Fatalf("the refund downgraded the plan itself (%q); that is the lapse job's decision", s.updatedTenant.Plan)
	}
}

// TestPartialRefundLeavesThePlanAlone: the customer kept part of what they paid
// for, so taking the whole month back would be a second penalty nobody agreed
// to.
func TestPartialRefundLeavesThePlanAlone(t *testing.T) {
	expires := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	s := &fakeStore{
		due: []store.Refund{pendingRefund(300)}, receipt: paidReceipt(),
		tenant: &store.Tenant{ID: "t1", Plan: "crew", PlanExpiresAt: &expires},
	}
	iss := &fakeIssuer{result: &invoiceninja.CreditResult{
		CreditID: "credit-1", CreditNumber: "MSHCN2026-0001", InvoiceNumber: "MSH2026-0001"}}
	j := newJob(s, iss)
	j.Once(context.Background())

	if s.updatedTenant != nil {
		t.Fatal("a partial refund shortened the plan by a whole period")
	}
	if len(s.issued) != 1 {
		t.Fatalf("the partial refund produced no document: %v", s.issued)
	}
}

// TestRefundNeverGivesAnOperatorSetTierAnExpiry: a tier an operator set by hand
// has no expiry and never lapses. A Ko-fi refund must not silently convert that
// into a plan with an end date.
func TestRefundNeverGivesAnOperatorSetTierAnExpiry(t *testing.T) {
	s := &fakeStore{
		due: []store.Refund{pendingRefund(900)}, receipt: paidReceipt(),
		tenant: &store.Tenant{ID: "t1", Plan: "custom", PlanExpiresAt: nil},
	}
	iss := &fakeIssuer{result: &invoiceninja.CreditResult{
		CreditID: "credit-1", CreditNumber: "MSHCN2026-0001"}}
	j := newJob(s, iss)
	j.Once(context.Background())

	if s.updatedTenant != nil {
		t.Fatal("a refund put an expiry on a plan an operator set by hand")
	}
}

// TestCreditIdIsPersistedBeforeTheDocumentIsNumbered. A sent credit note has
// taken a number out of a gapless series; a retry that created a second one
// would leave a credit note in the books that no refund ever matched.
func TestCreditIdIsPersistedBeforeTheDocumentIsNumbered(t *testing.T) {
	s := &fakeStore{due: []store.Refund{pendingRefund(900)}, receipt: paidReceipt()}
	iss := &fakeIssuer{result: &invoiceninja.CreditResult{
		CreditID: "credit-new", CreditNumber: "MSHCN2026-0001"}}
	j := newJob(s, iss)
	j.Once(context.Background())

	if len(s.setCredits) != 1 || s.setCredits[0] != "rf-1=credit-new" {
		t.Fatalf("the credit note id was not persisted the moment it existed: %v", s.setCredits)
	}
}

// TestResumeUsesTheExistingCreditNote. A refund that already has a credit_ref
// must resume on that document, never create a second one.
func TestResumeUsesTheExistingCreditNote(t *testing.T) {
	r := pendingRefund(900)
	r.CreditRef = "credit-earlier"
	s := &fakeStore{due: []store.Refund{r}, receipt: paidReceipt()}
	iss := &fakeIssuer{result: &invoiceninja.CreditResult{
		CreditID: "credit-earlier", CreditNumber: "MSHCN2026-0001"}}
	j := newJob(s, iss)
	j.Once(context.Background())

	if iss.req.ExistingCreditID != "credit-earlier" {
		t.Fatalf("a resumed refund did not carry its existing credit note (%q); that is a second document",
			iss.req.ExistingCreditID)
	}
}

func TestTerminalCausesParkAndRetryableOnesRetry(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		park bool
	}{
		{"invoice gone", invoiceninja.ErrInvoiceGone, true},
		{"wrong currency", invoiceninja.ErrWrongCurrency, true},
		{"bad amount", invoiceninja.ErrBadAmount, true},
		{"422 from the billing system", &invoiceninja.Error{Status: http.StatusUnprocessableEntity, Op: "x"}, true},
		{"503 from the billing system", &invoiceninja.Error{Status: http.StatusServiceUnavailable, Op: "x"}, false},
		{"connection refused", errors.New("dial tcp: connection refused"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &fakeStore{due: []store.Refund{pendingRefund(900)}, receipt: paidReceipt()}
			j := newJob(s, &fakeIssuer{err: tc.err})
			j.Once(context.Background())

			if tc.park {
				if len(s.blocked) != 1 {
					t.Fatalf("a terminal cause looped instead of parking: blocked=%v attempts=%v", s.blocked, s.attempts)
				}
			} else if len(s.attempts) != 1 || len(s.blocked) != 0 {
				t.Fatalf("a temporary failure was parked instead of retried: blocked=%v attempts=%v", s.blocked, s.attempts)
			}
		})
	}
}

// TestTheRowIsClaimedBeforeTheBillingSystemIsTouched. The lease is what stops
// two drainers drawing two credit note numbers for one refund, and it must be
// taken before anything irreversible happens -- not after.
func TestTheRowIsClaimedBeforeTheBillingSystemIsTouched(t *testing.T) {
	s := &fakeStore{due: []store.Refund{pendingRefund(900)}, receipt: paidReceipt(), claimAll: true}
	iss := &fakeIssuer{}
	j := newJob(s, iss)
	j.Once(context.Background())

	if len(s.claimed) != 1 {
		t.Fatalf("the row was not claimed: %v", s.claimed)
	}
	if iss.calls != 0 {
		t.Fatal("a refund whose lease was lost still touched the billing system")
	}
	if len(s.released) != 0 {
		t.Fatal("a drainer released a lease it never held")
	}
}

// TestTheCustomerGetsTheDocumentAndTheWords. The whole point: money went back
// and the customer hears about it from us, with the credit note attached.
func TestTheCustomerGetsTheDocumentAndTheWords(t *testing.T) {
	expires := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	s := &fakeStore{
		due: []store.Refund{pendingRefund(900)}, receipt: paidReceipt(),
		tenant: &store.Tenant{ID: "t1", Plan: "crew", PlanExpiresAt: &expires},
	}
	iss := &fakeIssuer{result: &invoiceninja.CreditResult{
		CreditID: "credit-1", CreditNumber: "MSHCN2026-0001", InvoiceNumber: "MSH2026-0001"}}
	m := &fakeMailer{}
	j := newJob(s, iss)
	j.SetMailer(m, "https://hub.meshsat.net")
	j.Once(context.Background())

	if len(m.attached) != 1 {
		t.Fatalf("the credit note did not travel with the message: %v", m.sent)
	}
	if m.attached[0].Filename != "MSHCN2026-0001.pdf" || m.attached[0].ContentType != "application/pdf" {
		t.Fatalf("the attachment is not the document: %+v", m.attached[0])
	}
	if !strings.Contains(m.bodies[0], "MSH2026-0001") {
		t.Fatal("the notice does not say which invoice it corrects")
	}
	// The plan sentence must state what is true after the refund, not before.
	if !strings.Contains(m.bodies[0], "Thursday 10-Sep-2026") {
		t.Fatalf("the notice does not state the plan's new end:\n%s", m.bodies[0])
	}
	if !strings.Contains(m.bodies[0], "an SOS is never affected by billing") {
		t.Fatal("the notice drops the SOS assurance")
	}
}

// TestAPdfFailureStillTellsTheCustomer. The document exists and is in the
// books; re-running the issue path to fix an email would risk the document
// rather than the message.
func TestAPdfFailureStillTellsTheCustomer(t *testing.T) {
	s := &fakeStore{due: []store.Refund{pendingRefund(900)}, receipt: paidReceipt()}
	iss := &fakeIssuer{
		result: &invoiceninja.CreditResult{CreditID: "credit-1", CreditNumber: "MSHCN2026-0001"},
		pdfErr: errors.New("billing system down"),
	}
	m := &fakeMailer{}
	j := newJob(s, iss)
	j.SetMailer(m, "https://hub.meshsat.net")
	j.Once(context.Background())

	if len(s.issued) != 1 {
		t.Fatal("a failed PDF fetch un-issued a document that exists")
	}
	if len(m.sent) != 1 || len(m.attached) != 0 {
		t.Fatalf("the customer was told nothing when the PDF could not be fetched: %v", m.sent)
	}
}

func TestFilenameIsSafeForAHeader(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"MSHCN2026-0001", "MSHCN2026-0001.pdf"},
		{`MSH"CN 2026/0001`, "MSHCN20260001.pdf"},
		{"", "credit-note.pdf"},
	} {
		if got := filename(tc.in); got != tc.want {
			t.Fatalf("filename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBackoffGrowsAndStops(t *testing.T) {
	if backoff(0) != time.Minute {
		t.Fatalf("backoff(0) = %v", backoff(0))
	}
	if backoff(3) != 8*time.Minute {
		t.Fatalf("backoff(3) = %v", backoff(3))
	}
	if backoff(99) != maxBackoff {
		t.Fatalf("backoff(99) = %v, want the cap", backoff(99))
	}
}

// TestARetryDoesNotShortenThePlanTwice pins the ordering that makes the plan
// reversal safe, and it is not a hypothetical.
//
// Taking the paid period back is the one step here that is NOT idempotent:
// subtracting a month from a date subtracts a month every time it runs. So it
// must happen only AFTER the refund is recorded as issued, because that record
// is what stops the drainer running the whole thing again. Reversing first and
// recording second means one failed write costs the customer a second month.
func TestARetryDoesNotShortenThePlanTwice(t *testing.T) {
	expires := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	tenant := &store.Tenant{ID: "t1", Plan: "crew", PlanExpiresAt: &expires}
	s := &fakeStore{
		due: []store.Refund{pendingRefund(900)}, receipt: paidReceipt(), tenant: tenant,
		issueErr: errors.New("database went away"),
	}
	iss := &fakeIssuer{result: &invoiceninja.CreditResult{
		CreditID: "credit-1", CreditNumber: "MSHCN2026-0001"}}
	j := newJob(s, iss)
	j.Once(context.Background())

	if s.updatedTenant != nil {
		t.Fatalf("the plan was shortened before the refund was recorded; "+
			"the next pass shortens it again. expiry moved to %v", s.updatedTenant.PlanExpiresAt)
	}

	// The retry succeeds, and the period comes back exactly once.
	s.issueErr = nil
	s.due = []store.Refund{pendingRefund(900)}
	s.due[0].CreditRef = "credit-1"
	j.Once(context.Background())

	if s.updatedTenant == nil {
		t.Fatal("the successful pass never took the paid period back")
	}
	want := expires.Add(-kofi.Period)
	if !s.updatedTenant.PlanExpiresAt.Equal(want) {
		t.Fatalf("expiry = %v, want %v: exactly one period, however many passes it took",
			s.updatedTenant.PlanExpiresAt, want)
	}
}

// The same ordering has to hold on the no-document path.
func TestARetryOnTheNoDocumentPathDoesNotShortenThePlanTwice(t *testing.T) {
	expires := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	rcpt := paidReceipt()
	rcpt.InvoiceRef = ""
	rcpt.Status = store.ReceiptPending
	s := &fakeStore{
		due: []store.Refund{pendingRefund(900)}, receipt: rcpt,
		tenant:   &store.Tenant{ID: "t1", Plan: "crew", PlanExpiresAt: &expires},
		issueErr: errors.New("database went away"),
	}
	j := newJob(s, &fakeIssuer{})
	j.Once(context.Background())

	if s.updatedTenant != nil {
		t.Fatal("the plan was shortened before the refund was recorded on the no-document path")
	}
}

// TestARaceWithTheReceiptDrainerNeverCancelsAnIssuedInvoice.
//
// Both drainers run on the lease holder, a minute apart, and both touch the
// same receipt row. An operator refunding a payment in the window before its
// invoice is issued makes them collide: the refund path reads "no invoice yet"
// and decides to cancel the receipt, while the receipt path is mid-flight and
// issues one. Losing that race two ways is expensive -- the customer is billed
// for money that has already gone back, and an issued receipt is flipped to
// blocked so nothing will ever credit it.
//
// So the decision is re-read at the last moment. If an invoice appeared, the
// refund goes back in the queue and the next pass takes the credit note path.
func TestARaceWithTheReceiptDrainerNeverCancelsAnIssuedInvoice(t *testing.T) {
	rcpt := paidReceipt()
	rcpt.InvoiceRef = "" // as the refund path first sees it
	rcpt.Status = store.ReceiptPending
	s := &fakeStore{
		due: []store.Refund{pendingRefund(900)}, receipt: rcpt,
		tenant: &store.Tenant{ID: "t1", Plan: "crew"},
	}
	// The receipt drainer wins the race between the two reads.
	s.onSecondGetReceipt = func() {
		issued := paidReceipt()
		issued.Status = store.ReceiptIssued
		s.receipt = issued
	}
	iss := &fakeIssuer{}
	j := newJob(s, iss)
	j.Once(context.Background())

	if len(s.blockedRcpts) != 0 {
		t.Fatalf("an issued receipt was cancelled by the refund path: %v", s.blockedRcpts)
	}
	if len(s.issued) != 0 {
		t.Fatal("the refund was closed with no document even though an invoice exists")
	}
	if len(s.attempts) != 1 {
		t.Fatalf("the refund was not put back for the credit note path: %v", s.attempts)
	}
	if s.updatedTenant != nil {
		t.Fatal("the plan was shortened on a pass that resolved nothing")
	}
}
