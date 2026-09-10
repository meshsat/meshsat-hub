package kofi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/invoiceninja"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// Receipts: turning a payment into a document (MESHSAT-998).
//
// Ko-fi has been charging for MeshSat Hub plans since the launch and nothing
// produced a receipt. The money landed in a payment processor, the Hub granted
// a plan, and no document was ever issued to the customer or into the books.
//
// The shape here is an outbox, and it is an outbox rather than a call in the
// webhook handler for one reason: the billing system is a separate machine
// that can be down, and a webhook that returns 500 because a receipt could not
// be issued would make Ko-fi retry the whole payment. So the handler records
// that money arrived -- one row, one unique delivery key, one insert -- and a
// lease-held drainer issues the document, retrying until it succeeds.
//
// The row is written BEFORE the plan is granted, because the receipt records
// the payment, not the grant. If granting the plan fails, the money still
// arrived and the customer is still owed a document.

// ReceiptStore is the slice of the store the receipt outbox needs.
type ReceiptStore interface {
	CreateReceipt(ctx context.Context, r *store.Receipt) (bool, error)
	GetReceiptByKey(ctx context.Context, deliveryKey string) (*store.Receipt, error)
	ListDueReceipts(ctx context.Context, now time.Time, limit int) ([]store.Receipt, error)
	SetReceiptInvoice(ctx context.Context, id, invoiceRef string) error
	MarkReceiptIssued(ctx context.Context, id, invoiceNumber, invoiceRef string, at time.Time) error
	MarkReceiptAttempt(ctx context.Context, id, errMsg string, nextAttempt time.Time) error
	BlockReceipt(ctx context.Context, id, reason string) error
	ClaimReceipt(ctx context.Context, id string, until time.Time) (bool, error)
	ReleaseReceipt(ctx context.Context, id string) error
}

// Issuer is the billing system. Satisfied by *invoiceninja.Client.
type Issuer interface {
	IssueReceipt(ctx context.Context, req invoiceninja.Request) (*invoiceninja.Result, error)
}

// deliveryKey is the idempotency token for one payment.
//
// Ko-fi's message_id is the right key when there is one: Ko-fi retries a
// delivery with the same id until it gets a 200. But the field is a third
// party's and can be absent, and an absent id must not become an empty string
// that collides with every other empty string -- that would dedupe unrelated
// payments into a single document. So it falls back to the transaction id, and
// then to a digest of the payment's own facts, which still catches a replay of
// an identical delivery.
func deliveryKey(p Payload) string {
	if id := strings.TrimSpace(p.MessageID); id != "" {
		return id
	}
	if id := strings.TrimSpace(p.KofiTransactionID); id != "" {
		return "txn:" + id
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		p.Email, p.Amount, p.Currency, p.Timestamp, p.TierName,
	}, "\x00")))
	return "digest:" + hex.EncodeToString(sum[:16])
}

// recordReceipt writes the outbox row for a matched payment. It never returns
// an error to the caller's HTTP response: a failure here is loud in the log
// and the delivery is still acknowledged, because refusing the webhook would
// make Ko-fi replay a payment that has already been applied.
func (h *Handler) recordReceipt(ctx context.Context, t *store.Tenant, p Payload, plan string) {
	if h.receipts == nil {
		return
	}
	key := deliveryKey(p)
	cents, err := invoiceninja.ParseAmount(p.Amount)
	if err != nil {
		slog.Error("kofi: payment amount could not be read, no receipt will be issued",
			"tenant", t.ID, "txn", p.KofiTransactionID, "error", err)
		return
	}

	r := &store.Receipt{
		TenantID:      t.ID,
		DeliveryKey:   key,
		TransactionID: p.KofiTransactionID,
		Email:         h.receiptEmail(ctx, t, p),
		Name:          t.Name,
		AmountCents:   cents,
		Currency:      strings.ToUpper(strings.TrimSpace(p.Currency)),
		Plan:          plan,
		TierName:      p.TierName,
		PaidAt:        paidAt(p),
	}
	created, err := h.receipts.CreateReceipt(ctx, r)
	if err != nil {
		// The customer is owed a document and this is the only record that
		// they are. Nothing downstream can recover it, so it is an error.
		slog.Error("kofi: could not record that a payment owes a receipt",
			"tenant", t.ID, "txn", p.KofiTransactionID, "error", err)
		return
	}
	if !created {
		slog.Info("kofi: receipt for this delivery already recorded", "tenant", t.ID, "txn", p.KofiTransactionID)
		return
	}
	slog.Info("kofi: payment recorded for a receipt", "tenant", t.ID, "receipt", r.ID,
		"amount_cents", cents, "currency", r.Currency, "txn", p.KofiTransactionID)
}

// receiptEmail is where the document goes: the account owner's address, which
// is the one the customer uses with us and the one that stays stable across a
// change of payment card, falling back to the address Ko-fi says paid.
func (h *Handler) receiptEmail(ctx context.Context, t *store.Tenant, p Payload) string {
	if h.users != nil && t.OwnerUserID != "" {
		if u, err := h.users.GetUserByID(ctx, t.ID, t.OwnerUserID); err == nil && u != nil && u.Email != "" {
			return strings.ToLower(strings.TrimSpace(u.Email))
		}
	}
	return strings.ToLower(strings.TrimSpace(p.Email))
}

// paidAt is when the money arrived. Ko-fi's own timestamp when it parses,
// otherwise now -- a receipt dated today is a smaller problem than no receipt.
func paidAt(p Payload) time.Time {
	if ts, err := time.Parse(time.RFC3339, strings.TrimSpace(p.Timestamp)); err == nil {
		return ts.UTC()
	}
	return time.Now().UTC()
}

// ReceiptJob issues the documents the webhook recorded.
//
// It runs on the lease holder, like the lapse job, because issuing an invoice
// twice is worse than issuing it late and because the number series is a
// single sequence. A failed attempt backs off and stays pending: money that
// arrived is never dropped from this queue, only parked by BlockReceipt when
// a person has to decide something.
type ReceiptJob struct {
	store  ReceiptStore
	issuer Issuer
	audit  Auditor
	every  time.Duration
	batch  int
	now    func() time.Time
}

// NewReceiptJob returns the drainer.
func NewReceiptJob(s ReceiptStore, issuer Issuer, a Auditor) *ReceiptJob {
	return &ReceiptJob{store: s, issuer: issuer, audit: a, every: time.Minute, batch: 25, now: time.Now}
}

// Run drains the outbox until the context is cancelled.
func (j *ReceiptJob) Run(ctx context.Context) {
	t := time.NewTicker(j.every)
	defer t.Stop()
	j.Once(ctx) // catch anything recorded while nobody held the lease
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.Once(ctx)
		}
	}
}

// Once performs a single pass. Exported so a test can drive it with a clock.
func (j *ReceiptJob) Once(ctx context.Context) {
	now := j.now().UTC()
	due, err := j.store.ListDueReceipts(ctx, now, j.batch)
	if err != nil {
		slog.Error("kofi: listing receipts to issue failed", "error", err)
		return
	}
	for i := range due {
		if ctx.Err() != nil {
			return
		}
		r := &due[i]
		// Claim the row before the billing system is touched at all. The lease
		// is what stops two drainers drawing two invoice numbers for one
		// payment, and it does not depend on the leader handover timing being
		// generous enough -- a drainer that dies mid-issue just lets the lease
		// expire (MESHSAT-998).
		won, err := j.store.ClaimReceipt(ctx, r.ID, now.Add(receiptLease))
		if err != nil {
			slog.Error("kofi: could not claim a receipt; leaving it for the next pass",
				"receipt", r.ID, "error", err)
			continue
		}
		if !won {
			slog.Info("kofi: another drainer holds this receipt", "receipt", r.ID)
			continue
		}
		j.issue(ctx, r)
		if err := j.store.ReleaseReceipt(ctx, r.ID); err != nil {
			slog.Warn("kofi: could not release a receipt lease; it expires on its own",
				"receipt", r.ID, "error", err)
		}
	}
}

// receiptLease bounds how long one drainer may hold a receipt row. Long enough
// to cover a billing call that runs to its 30 s timeout with room to spare,
// short enough that a drainer killed mid-issue does not strand the customer's
// document for long.
const receiptLease = 5 * time.Minute

// maxBackoff caps the retry interval. There is no attempt limit: a receipt is
// a document somebody is owed for money already taken, so it is never
// abandoned, only slowed down.
const maxBackoff = time.Hour

func (j *ReceiptJob) issue(ctx context.Context, r *store.Receipt) {
	if r.Email == "" {
		j.block(ctx, r, "no email address to send the receipt to")
		return
	}
	req := invoiceninja.Request{
		CustomerRef:       r.TenantID,
		Name:              r.Name,
		Email:             r.Email,
		AmountCents:       r.AmountCents,
		Currency:          r.Currency,
		ProductKey:        productKey(r.Plan),
		Description:       description(r.Plan, r.TierName),
		PaidAt:            r.PaidAt,
		Reference:         r.TransactionID,
		ExistingInvoiceID: r.InvoiceRef,
		OnInvoiceCreated: func(invoiceID string) error {
			// Written before the invoice is numbered or paid. An invoice that
			// has been sent has taken a number out of a gapless series, so a
			// retry that created a second one would leave the first
			// permanently unpaid and the books wrong. Returning an error here
			// stops the client before it sends the invoice.
			return j.store.SetReceiptInvoice(ctx, r.ID, invoiceID)
		},
	}
	res, err := j.issuer.IssueReceipt(ctx, req)
	if err != nil {
		// Parked, not retried: repeating either of these produces the same
		// answer. A wrong currency must never be converted at an invented rate,
		// and a zero or negative amount is a payload a person has to look at --
		// it used to sit in the queue retrying every hour, forever, silently.
		if errors.Is(err, invoiceninja.ErrWrongCurrency) || errors.Is(err, invoiceninja.ErrBadAmount) {
			j.block(ctx, r, err.Error())
			return
		}
		var apiErr *invoiceninja.Error
		if errors.As(err, &apiErr) && !apiErr.Retryable() {
			// The request itself is wrong. Repeating it produces the same
			// answer, so park it for a person instead of a loop.
			j.block(ctx, r, err.Error())
			return
		}
		next := j.now().UTC().Add(backoff(r.Attempts))
		if err2 := j.store.MarkReceiptAttempt(ctx, r.ID, err.Error(), next); err2 != nil {
			slog.Error("kofi: could not record a failed receipt attempt", "receipt", r.ID, "error", err2)
		}
		slog.Warn("kofi: issuing a receipt failed, will retry", "receipt", r.ID, "tenant", r.TenantID,
			"attempts", r.Attempts+1, "retry_at", next.Format(time.RFC3339), "error", err)
		return
	}

	if err := j.store.MarkReceiptIssued(ctx, r.ID, res.InvoiceNumber, res.InvoiceID, j.now().UTC()); err != nil {
		// The document exists and the customer has it; only our record of that
		// is missing. Re-issuing would be caught by the balance check, so the
		// worst case of a retry is a wasted call, not a second invoice.
		slog.Error("kofi: receipt issued but recording it failed",
			"receipt", r.ID, "invoice", res.InvoiceNumber, "error", err)
		return
	}
	slog.Info("kofi: receipt issued", "receipt", r.ID, "tenant", r.TenantID, "invoice", res.InvoiceNumber)
	if j.audit != nil {
		_ = j.audit.Log(ctx, r.TenantID, "receipt_issued", "receipt_job",
			fmt.Sprintf("invoice %s for %s %s", res.InvoiceNumber, money(r.AmountCents), r.Currency), "")
	}
}

func (j *ReceiptJob) block(ctx context.Context, r *store.Receipt, reason string) {
	if err := j.store.BlockReceipt(ctx, r.ID, reason); err != nil {
		slog.Error("kofi: could not park a receipt", "receipt", r.ID, "error", err)
		return
	}
	// Loud: money arrived and no document went out. Nothing else will notice.
	slog.Error("kofi: a paid subscription has no receipt and needs an operator",
		"receipt", r.ID, "tenant", r.TenantID, "amount_cents", r.AmountCents,
		"currency", r.Currency, "reason", reason)
}

// backoff grows with the attempt count and stops at maxBackoff.
func backoff(attempts int) time.Duration {
	d := time.Minute
	for i := 0; i < attempts && d < maxBackoff; i++ {
		d *= 2
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// money renders minor units for a log or an audit line.
func money(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}

func productKey(plan string) string {
	if plan == "" {
		return "MeshSat Hub subscription"
	}
	return "MeshSat Hub " + strings.ToUpper(plan[:1]) + plan[1:]
}

func description(plan, tierName string) string {
	name := tierName
	if name == "" {
		name = plan
	}
	if name == "" {
		return "MeshSat Hub subscription, one month"
	}
	return "MeshSat Hub subscription, " + name + ", one month"
}
