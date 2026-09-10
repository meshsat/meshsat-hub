// Package refunds turns money given back into the document that reverses the
// sale (MESHSAT-1019).
//
// The gap this closes was total. The terms promise EU consumers 14 days to
// withdraw from a subscription and get their money back, and a Ko-fi payment is
// refundable in the payment processor -- but nothing produced a document when
// that happened. The processor's refund emails are off by deliberate setting;
// the billing system had no credit-note flow wired at all. So a refunded
// customer got their money back and nothing else, and the sale stayed in the
// books at full value with its VAT declared to the tax authority.
//
// Three decisions shape this package, and each of them is a constraint rather
// than a preference:
//
//   - The money moves by hand, in the payment processor, so the trigger is an
//     operator recording that it moved. Nothing here refunds anybody: a Ko-fi
//     webhook never fires on a refund, and inferring one from a poll would put
//     this code in the position of issuing legal documents for an event it
//     guessed at.
//   - A credit note is a document with a number out of a gapless series, so the
//     work runs in an outbox on the lease holder, exactly like receipts. A
//     second credit note for one refund is worse than a late one.
//   - The customer's copy is sent from the Hub, not from the billing system.
//     Invoice Ninja 5.13.31 sends credit-note mail through a path with no
//     per-company sender, so a MeshSat customer would receive their credit note
//     from the other company on that instance -- wrong brand, wrong domain, and
//     it puts the legal entity in the From line, which belongs on the document
//     and not in the customer's inbox.
package refunds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/invoiceninja"
	"github.com/meshsat/meshsat-hub/internal/kofi"
	"github.com/meshsat/meshsat-hub/internal/mail"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// Store is the slice of the store this needs.
type Store interface {
	ListDueRefunds(ctx context.Context, now time.Time, limit int) ([]store.Refund, error)
	GetReceipt(ctx context.Context, id string) (*store.Receipt, error)
	SetRefundCredit(ctx context.Context, id, creditRef string) error
	MarkRefundIssued(ctx context.Context, id, creditNumber, creditRef string, at time.Time) error
	MarkRefundAttempt(ctx context.Context, id, errMsg string, nextAttempt time.Time) error
	BlockRefund(ctx context.Context, id, reason string) error
	ClaimRefund(ctx context.Context, id string, until time.Time) (bool, error)
	ReleaseRefund(ctx context.Context, id string) error
	BlockReceipt(ctx context.Context, id, reason string) error
	GetTenant(ctx context.Context, id string) (*store.Tenant, error)
	UpdateTenant(ctx context.Context, t *store.Tenant) error
}

// Issuer is the billing system. Satisfied by *invoiceninja.Client.
type Issuer interface {
	IssueCreditNote(ctx context.Context, req invoiceninja.CreditRequest) (*invoiceninja.CreditResult, error)
	CreditPDF(ctx context.Context, creditID string) ([]byte, error)
}

// Auditor records what was done to whose money. Matches audit.Service.Log.
type Auditor interface {
	Log(ctx context.Context, tenantID, action, actor, detail, ip string) error
}

// Mailer sends the customer their copy. Two interfaces because the credit note
// travels as an attachment and the no-document notice does not.
type Mailer interface {
	mail.Sender
	mail.AttachmentSender
}

// Job issues the credit notes an operator recorded.
//
// It runs on the lease holder for the same reason the receipt job does: the
// credit note series is a single sequence, and issuing one twice is worse than
// issuing it late.
type Job struct {
	store  Store
	issuer Issuer
	audit  Auditor
	mail   Mailer
	hubURL string
	every  time.Duration
	batch  int
	now    func() time.Time
	// forget drops the tenant's cached record on the other replicas, so a plan
	// that a refund shortened applies at once rather than at the end of a TTL.
	forget func(tenantID string)
}

// New returns the drainer.
func New(s Store, issuer Issuer, a Auditor) *Job {
	return &Job{store: s, issuer: issuer, audit: a, every: time.Minute, batch: 25, now: time.Now}
}

// SetMailer gives the job a way to send the customer their credit note. Without
// one the document is still produced and recorded; only the customer's copy is
// missing, which a person can send by hand.
func (j *Job) SetMailer(m Mailer, hubURL string) { j.mail, j.hubURL = m, hubURL }

// SetInvalidator wires the tenant-status cache so a shortened plan takes effect
// on every replica at once.
func (j *Job) SetInvalidator(forget func(tenantID string)) { j.forget = forget }

// refundLease bounds how long one drainer may hold a refund row. Longer than
// the receipt lease because issuing a credit note is four calls to the billing
// system rather than three, and a lease that expires mid-sequence is how two
// drainers end up in it at once.
const refundLease = 10 * time.Minute

// maxBackoff caps the retry interval. There is no attempt limit: the customer
// has had their money back and is owed the document, so it is never abandoned,
// only slowed down.
const maxBackoff = time.Hour

// Run drains the outbox until the context is cancelled.
func (j *Job) Run(ctx context.Context) {
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
func (j *Job) Once(ctx context.Context) {
	now := j.now().UTC()
	due, err := j.store.ListDueRefunds(ctx, now, j.batch)
	if err != nil {
		slog.Error("refunds: listing refunds to issue failed", "error", err)
		return
	}
	for i := range due {
		if ctx.Err() != nil {
			return
		}
		r := &due[i]
		won, err := j.store.ClaimRefund(ctx, r.ID, now.Add(refundLease))
		if err != nil {
			slog.Error("refunds: could not claim a refund; leaving it for the next pass",
				"refund", r.ID, "error", err)
			continue
		}
		if !won {
			slog.Info("refunds: another drainer holds this refund", "refund", r.ID)
			continue
		}
		j.issue(ctx, r)
		if err := j.store.ReleaseRefund(ctx, r.ID); err != nil {
			slog.Warn("refunds: could not release a refund lease; it expires on its own",
				"refund", r.ID, "error", err)
		}
	}
}

func (j *Job) issue(ctx context.Context, r *store.Refund) {
	receipt, err := j.store.GetReceipt(ctx, r.ReceiptID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			j.block(ctx, r, "the payment this refund reverses is no longer in the database")
			return
		}
		j.retry(ctx, r, err)
		return
	}

	// A payment refunded before its receipt was ever issued has no invoice to
	// credit. Cancelling the receipt is the whole of the correct action: the
	// customer is owed nothing, and leaving the receipt pending would have the
	// other drainer issue an invoice for money that has already gone back.
	if receipt.InvoiceRef == "" {
		j.closeWithoutDocument(ctx, r, receipt)
		return
	}

	res, err := j.issuer.IssueCreditNote(ctx, invoiceninja.CreditRequest{
		InvoiceID:        receipt.InvoiceRef,
		AmountCents:      r.AmountCents,
		Currency:         r.Currency,
		ProductKey:       productKey(receipt.Plan),
		Description:      description(receipt.Plan, receipt.TierName),
		RefundedAt:       r.RefundedAt,
		ExistingCreditID: r.CreditRef,
		OnCreditCreated: func(creditID string) error {
			// Persisted the instant it exists, before it is numbered. A sent
			// credit note has taken a number out of a gapless series, so a
			// retry that created a second one would leave a credit note in the
			// books that no refund ever matched.
			return j.store.SetRefundCredit(ctx, r.ID, creditID)
		},
	})
	if err != nil {
		// Terminal causes: repeating them produces the same answer, and a
		// document at the wrong figure cannot be deleted afterwards.
		if errors.Is(err, invoiceninja.ErrInvoiceGone) ||
			errors.Is(err, invoiceninja.ErrWrongCurrency) ||
			errors.Is(err, invoiceninja.ErrBadAmount) {
			j.block(ctx, r, err.Error())
			return
		}
		var apiErr *invoiceninja.Error
		if errors.As(err, &apiErr) && !apiErr.Retryable() {
			j.block(ctx, r, err.Error())
			return
		}
		j.retry(ctx, r, err)
		return
	}

	// Recording the refund comes FIRST, and the order is the whole guard.
	//
	// Taking the paid period back is the one step here that is not idempotent:
	// subtracting a month from a date subtracts a month every time it runs. This
	// record is what stops the drainer running the pass again, so it has to be
	// written before anything unrepeatable happens. Reversing the plan first and
	// recording second means one failed write costs the customer a second month.
	if err := j.store.MarkRefundIssued(ctx, r.ID, res.CreditNumber, res.CreditID, j.now().UTC()); err != nil {
		// The credit note exists; only our record of it is missing. A retry
		// resumes on the same document rather than creating another, so the
		// worst case is a wasted pass -- and the plan has not moved yet.
		slog.Error("refunds: credit note issued but recording it failed",
			"refund", r.ID, "credit", res.CreditNumber, "error", err)
		return
	}
	// Now the period, and then the customer, so the message states what is true
	// rather than what is about to be.
	planEnds := j.reversePlan(ctx, r, receipt)
	slog.Info("refunds: credit note issued", "refund", r.ID, "tenant", r.TenantID,
		"credit", res.CreditNumber, "invoice", res.InvoiceNumber,
		"amount_cents", r.AmountCents, "currency", r.Currency)
	if j.audit != nil {
		_ = j.audit.Log(ctx, r.TenantID, "refund_credited", "refund_job",
			fmt.Sprintf("credit note %s for %s %s against invoice %s",
				res.CreditNumber, money(r.AmountCents), r.Currency, res.InvoiceNumber), "")
	}
	j.notify(ctx, r, receipt, res, planEnds)
}

// closeWithoutDocument handles a refund of a payment that never became an
// invoice. It is a complete outcome, not a failure: nothing is owed, so nothing
// is issued, and the receipt is parked so the other drainer cannot bill for
// money that has gone back.
func (j *Job) closeWithoutDocument(ctx context.Context, r *store.Refund, receipt *store.Receipt) {
	if receipt.Status != store.ReceiptBlocked {
		if err := j.store.BlockReceipt(ctx, receipt.ID,
			"the payment was refunded before any invoice was issued, so no document is owed"); err != nil {
			j.retry(ctx, r, fmt.Errorf("cancelling the receipt: %w", err))
			return
		}
	}
	// Recorded before the period is taken back, for the reason in issue(): the
	// reversal is the one step that is not idempotent, and this record is what
	// stops the pass running again.
	if err := j.store.MarkRefundIssued(ctx, r.ID, "", "", j.now().UTC()); err != nil {
		j.retry(ctx, r, fmt.Errorf("recording the refund: %w", err))
		return
	}
	planEnds := j.reversePlan(ctx, r, receipt)
	slog.Info("refunds: refund closed with no credit note; the payment had no invoice",
		"refund", r.ID, "tenant", r.TenantID, "receipt", receipt.ID)
	if j.audit != nil {
		_ = j.audit.Log(ctx, r.TenantID, "refund_credited", "refund_job",
			fmt.Sprintf("%s %s refunded before any invoice existed; receipt %s cancelled",
				money(r.AmountCents), r.Currency, receipt.ID), "")
	}
	if j.mail != nil && receipt.Email != "" {
		subject, body := mail.RefundedNoDocument(receipt.Name, money(r.AmountCents)+" "+r.Currency, planEnds, j.hubURL)
		mail.SendOrLog(ctx, j.mail, receipt.Email, subject, body, "refund, no document")
	}
}

// reversePlan takes back the time the refunded payment bought.
//
// Only a full refund moves the plan: a partial one means the customer kept part
// of what they paid for, and shortening the period by the whole month for a
// third of the money back would be a second penalty nobody agreed to.
//
// It never downgrades anything itself. Pulling the expiry back is enough -- the
// hourly lapse job drops the plan when the date passes, which keeps one piece
// of code deciding what a lapse means. And nothing here touches devices,
// ingest or SOS: the ceiling gates registration and nothing else.
//
// Returns the plan's new end, or the zero time when the plan was not moved.
func (j *Job) reversePlan(ctx context.Context, r *store.Refund, receipt *store.Receipt) time.Time {
	if r.AmountCents < receipt.AmountCents {
		return time.Time{} // partial refund: the plan stands
	}
	t, err := j.store.GetTenant(ctx, r.TenantID)
	if err != nil || t == nil {
		slog.Warn("refunds: could not read the tenant to shorten its plan",
			"refund", r.ID, "tenant", r.TenantID, "error", err)
		return time.Time{}
	}
	if t.PlanExpiresAt == nil {
		// No expiry means an operator set this tier by hand and it does not
		// lapse. A Ko-fi refund must not silently convert that into a plan
		// with an end date.
		return time.Time{}
	}
	shortened := t.PlanExpiresAt.Add(-kofi.Period)
	t.PlanExpiresAt = &shortened
	if err := j.store.UpdateTenant(ctx, t); err != nil {
		slog.Error("refunds: could not shorten the plan after a refund",
			"refund", r.ID, "tenant", r.TenantID, "error", err)
		return time.Time{}
	}
	if j.forget != nil {
		j.forget(t.ID)
	}
	slog.Info("refunds: plan shortened by the refunded period", "refund", r.ID,
		"tenant", t.ID, "plan", t.Plan, "expires", shortened.Format(time.RFC3339))
	if j.audit != nil {
		_ = j.audit.Log(ctx, t.ID, "plan_period_reversed", "refund_job",
			"refund of "+money(r.AmountCents)+" "+r.Currency+
				" removed one paid period; expires "+shortened.Format(time.RFC3339), "")
	}
	return shortened
}

// notify sends the customer their credit note. A failure here is logged, never
// retried: the document exists and is in the books, and re-running the issue
// path to fix an email would risk the document rather than the message.
func (j *Job) notify(ctx context.Context, r *store.Refund, receipt *store.Receipt,
	res *invoiceninja.CreditResult, planEnds time.Time) {
	if j.mail == nil || receipt.Email == "" {
		return
	}
	subject, body := mail.Refunded(receipt.Name, money(r.AmountCents)+" "+r.Currency,
		res.CreditNumber, res.InvoiceNumber, planEnds, j.hubURL)
	pdf, err := j.issuer.CreditPDF(ctx, res.CreditID)
	if err != nil {
		// Send the words without the document rather than nothing at all. The
		// customer still learns their money went back and which credit note
		// covers it, and a person can send the PDF.
		slog.Error("refunds: could not fetch the credit note PDF; sending the notice without it",
			"refund", r.ID, "credit", res.CreditNumber, "error", err)
		mail.SendOrLog(ctx, j.mail, receipt.Email, subject, body, "refund notice")
		return
	}
	att := mail.Attachment{
		Filename:    filename(res.CreditNumber),
		ContentType: "application/pdf",
		Content:     pdf,
	}
	if err := j.mail.SendWith(ctx, receipt.Email, subject, body, att); err != nil {
		slog.Error("refunds: could not send the credit note", "refund", r.ID,
			"to", receipt.Email, "error", err)
		return
	}
	slog.Info("refunds: credit note sent", "refund", r.ID, "to", receipt.Email,
		"credit", res.CreditNumber)
}

func (j *Job) retry(ctx context.Context, r *store.Refund, cause error) {
	next := j.now().UTC().Add(backoff(r.Attempts))
	if err := j.store.MarkRefundAttempt(ctx, r.ID, cause.Error(), next); err != nil {
		slog.Error("refunds: could not record a failed attempt", "refund", r.ID, "error", err)
	}
	slog.Warn("refunds: issuing a credit note failed, will retry", "refund", r.ID,
		"tenant", r.TenantID, "attempts", r.Attempts+1,
		"retry_at", next.Format(time.RFC3339), "error", cause)
}

func (j *Job) block(ctx context.Context, r *store.Refund, reason string) {
	if err := j.store.BlockRefund(ctx, r.ID, reason); err != nil {
		slog.Error("refunds: could not park a refund", "refund", r.ID, "error", err)
		return
	}
	// Loud: money went back and no document followed it. Nothing else notices.
	slog.Error("refunds: a refund has no credit note and needs an operator",
		"refund", r.ID, "tenant", r.TenantID, "amount_cents", r.AmountCents,
		"currency", r.Currency, "reason", reason)
}

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

// money renders minor units for a log line, an audit line or a customer notice.
func money(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}

// productKey and description mirror the receipt's line, so the credit note
// reverses something a customer can recognise as what they bought.
func productKey(plan string) string {
	switch plan {
	case "":
		return "MeshSat Hub subscription"
	case kofi.DonationPlan:
		return "MeshSat Hub support"
	}
	return "MeshSat Hub " + strings.ToUpper(plan[:1]) + plan[1:]
}

func description(plan, tierName string) string {
	if plan == kofi.DonationPlan {
		return "Support for MeshSat Hub"
	}
	name := tierName
	if name == "" {
		name = plan
	}
	if name == "" {
		return "MeshSat Hub subscription, one month"
	}
	return "MeshSat Hub subscription, " + name + ", one month"
}

// filename names the attached PDF after the document, so a customer filing it
// does not have to open it to know what it is.
func filename(creditNumber string) string {
	n := strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return -1
	}, creditNumber)
	if n == "" {
		n = "credit-note"
	}
	return n + ".pdf"
}
