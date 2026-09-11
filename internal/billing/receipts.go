// Package billing turns a payment that has already happened into a document,
// and keeps a plan's expiry honest. It knows nothing about WHERE the money came
// from: a payment provider records a receipt row, and everything below works off
// that row.
//
// That split is the point. It lived in internal/kofi, which made a provider
// swap look like a rewrite of the receipt path when it is nothing of the kind
// (MESHSAT-1023).
//
// Receipts: turning a payment into a document (MESHSAT-998).
//
// The shape is an outbox, and it is an outbox rather than a call in the webhook
// handler for one reason: the billing system is a separate machine that can be
// down, and a webhook that returns 500 because a receipt could not be issued
// would make the payment provider retry the whole PAYMENT. So the handler
// records that money arrived -- one row, one unique delivery key, one insert --
// and a lease-held drainer issues the document, retrying until it succeeds.
//
// The row is written BEFORE the plan is granted, because the receipt records
// the payment, not the grant. If granting the plan fails, the money still
// arrived and the customer is still owed a document.
package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/invoiceninja"
	"github.com/meshsat/meshsat-hub/internal/mail"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/vat"
)

// Period is how long one payment buys when the provider does not tell us. A
// month plus two days: a membership is charged on the same day each month, the
// webhook can be late, and a tenant should never lapse because a renewal landed
// a few hours after the clock rolled over.
//
// A provider that reports its own period end (Stripe's current_period_end)
// should be believed instead of this. It remains the fallback, and refunds
// still reverse exactly one of these.
const Period = 32 * 24 * time.Hour

// Auditor records who was moved to which tier and why. Matches
// audit.Service.Log.
type Auditor interface {
	Log(ctx context.Context, tenantID, action, actor, detail, ip string) error
}

// PlanStore is the slice of the store the lapse job needs. Narrower than the
// payment provider's own store interface on purpose: nothing in here may grant
// a plan, only end one.
type PlanStore interface {
	ListTenants(ctx context.Context) ([]store.Tenant, error)
	UpdateTenant(ctx context.Context, t *store.Tenant) error
}

// UserLookup turns a tenant's owner into an account. Tenant.OwnerUserID holds a
// user id, never an address.
type UserLookup interface {
	GetUserByID(ctx context.Context, tenantID, id string) (*store.LocalUser, error)
}

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

// Mailer sends a donation's receipt from the Hub, with its PDF attached.
type Mailer interface {
	mail.Sender
	mail.AttachmentSender
}

// DocFetcher fetches a rendered invoice so the Hub can attach it. Kept off
// Issuer so an issuer that cannot do it still satisfies the interface.
type DocFetcher interface {
	InvoicePDF(ctx context.Context, invoiceID string) ([]byte, error)
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
	mail   Mailer
	docs   DocFetcher
	every  time.Duration
	batch  int
	now    func() time.Time
}

// NewReceiptJob returns the drainer.
func NewReceiptJob(s ReceiptStore, issuer Issuer, a Auditor) *ReceiptJob {
	return &ReceiptJob{store: s, issuer: issuer, audit: a, every: time.Minute, batch: 25, now: time.Now}
}

// SetMailer lets the job send a DONATION's receipt itself, with the PDF
// attached. Subscription receipts still come from the billing system, whose
// template is written for exactly that and is correct.
//
// Both arguments are needed together: without either, the job leaves the
// billing system's own email switched on. See selfNotifies.
func (j *ReceiptJob) SetMailer(m Mailer, docs DocFetcher) { j.mail, j.docs = m, docs }

// selfNotifies reports whether a donation's receipt can come from the Hub.
//
// This gates the suppression as well as the sending, and that is the whole
// point of it. Switching off the billing system's email without being able to
// send our own would mean money taken and the giver told nothing -- far worse
// than a message written for a subscription arriving about a gift.
func (j *ReceiptJob) selfNotifies() bool { return j.mail != nil && j.docs != nil }

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
		slog.Error("billing: listing receipts to issue failed", "error", err)
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
			slog.Error("billing: could not claim a receipt; leaving it for the next pass",
				"receipt", r.ID, "error", err)
			continue
		}
		if !won {
			slog.Info("billing: another drainer holds this receipt", "receipt", r.ID)
			continue
		}
		j.issue(ctx, r)
		if err := j.store.ReleaseReceipt(ctx, r.ID); err != nil {
			slog.Warn("billing: could not release a receipt lease; it expires on its own",
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
	// Where the buyer is decides whether Dutch VAT applies at all, and this is
	// asked BEFORE the billing system is touched: an invoice that has been sent
	// has taken a number out of a gapless series, so a document at the wrong
	// rate cannot simply be deleted afterwards (MESHSAT-1016).
	//
	// A donation is decided by what it buys, not by where the giver is, so it
	// takes a different rule and is never parked for a country -- see
	// vat.ForDonation.
	v := vat.For(r.Country)
	donation := r.Plan == DonationPlan
	if donation {
		v = vat.ForDonation()
	}
	if !v.Charge && !v.OutsideScope {
		j.block(ctx, r, v.Reason)
		return
	}
	req := invoiceninja.Request{
		CustomerRef: r.TenantID,
		CountryCode: r.Country,
		Name:        r.Name,
		Email:       r.Email,
		AmountCents: r.AmountCents,
		Currency:    r.Currency,
		TaxExempt:   v.OutsideScope,
		// The billing system has one payment template per company and it is
		// written for a subscription, so it tells a giver their subscription is
		// active and that the document shows the VAT included in the price --
		// neither of which is true of a gift. Send the donor's copy from here
		// instead, but only when we actually can.
		SuppressReceiptEmail: donation && j.selfNotifies(),
		ProductKey:           productKey(r.Plan),
		Description:          description(r.Plan, r.TierName),
		PaidAt:               r.PaidAt,
		Reference:            r.TransactionID,
		ExistingInvoiceID:    r.InvoiceRef,
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
			slog.Error("billing: could not record a failed receipt attempt", "receipt", r.ID, "error", err2)
		}
		slog.Warn("billing: issuing a receipt failed, will retry", "receipt", r.ID, "tenant", r.TenantID,
			"attempts", r.Attempts+1, "retry_at", next.Format(time.RFC3339), "error", err)
		return
	}

	if err := j.store.MarkReceiptIssued(ctx, r.ID, res.InvoiceNumber, res.InvoiceID, j.now().UTC()); err != nil {
		// The document exists and the customer has it; only our record of that
		// is missing. Re-issuing would be caught by the balance check, so the
		// worst case of a retry is a wasted call, not a second invoice.
		slog.Error("billing: receipt issued but recording it failed",
			"receipt", r.ID, "invoice", res.InvoiceNumber, "error", err)
		return
	}
	slog.Info("billing: receipt issued", "receipt", r.ID, "tenant", r.TenantID, "invoice", res.InvoiceNumber)
	if donation && j.selfNotifies() {
		j.notifyDonor(ctx, r, res)
	}
	if j.audit != nil {
		_ = j.audit.Log(ctx, r.TenantID, "receipt_issued", "receipt_job",
			fmt.Sprintf("invoice %s for %s %s", res.InvoiceNumber, money(r.AmountCents), r.Currency), "")
	}
}

// notifyDonor sends a giver their receipt from the Hub.
//
// A failure here is logged and never retried. The document exists, it is in the
// books and it is numbered; re-running the issue path to fix an email would
// risk the document rather than the message.
func (j *ReceiptJob) notifyDonor(ctx context.Context, r *store.Receipt, res *invoiceninja.Result) {
	msg := mail.DonationReceipt(r.Name,
		invoiceninja.FormatMoney(r.AmountCents, r.Currency, r.Country), res.InvoiceNumber)

	pdf, err := j.docs.InvoicePDF(ctx, res.InvoiceID)
	if err != nil {
		// Send the words without the document rather than nothing at all. The
		// giver still learns the money arrived and which receipt covers it, and
		// a person can send the PDF.
		slog.Error("billing: could not fetch the donation receipt PDF; sending the notice without it",
			"receipt", r.ID, "invoice", res.InvoiceNumber, "error", err)
		mail.SendOrLog(ctx, j.mail, r.Email, msg, "donation receipt")
		return
	}
	att := mail.Attachment{
		Filename:    pdfName(res.InvoiceNumber),
		ContentType: "application/pdf",
		Content:     pdf,
	}
	if err := j.mail.SendMessageWith(ctx, r.Email, msg, att); err != nil {
		slog.Error("billing: could not send the donation receipt", "receipt", r.ID,
			"to", r.Email, "error", err)
		return
	}
	slog.Info("billing: donation receipt sent", "receipt", r.ID, "to", r.Email,
		"invoice", res.InvoiceNumber)
}

// pdfName names the attachment after the document, so a giver filing it does
// not have to open it to know what it is. Anything that is not plainly safe in
// a filename is dropped rather than escaped.
func pdfName(invoiceNumber string) string {
	n := strings.Map(func(c rune) rune {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
			return c
		}
		return -1
	}, invoiceNumber)
	if n == "" {
		n = "receipt"
	}
	return n + ".pdf"
}

func (j *ReceiptJob) block(ctx context.Context, r *store.Receipt, reason string) {
	if err := j.store.BlockReceipt(ctx, r.ID, reason); err != nil {
		slog.Error("billing: could not park a receipt", "receipt", r.ID, "error", err)
		return
	}
	// Loud: money arrived and no document went out. Nothing else will notice.
	slog.Error("billing: a paid subscription has no receipt and needs an operator",
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

// DonationPlan marks a receipt for a one-off donation rather than a plan. It is
// not a plan and never grants one: it exists so the document does not describe
// a EUR 5 tip as "MeshSat Hub Free, subscription, one month", which is what it
// said the first time donations were invoiced at all.
//
// Defined in internal/store because two SQL queries have to recognise the same
// string; aliased here so every caller in the billing layer keeps reading
// naturally.
const DonationPlan = store.DonationPlan

func productKey(plan string) string {
	switch plan {
	case "":
		return "MeshSat Hub subscription"
	case DonationPlan:
		return "MeshSat Hub support"
	}
	return "MeshSat Hub " + strings.ToUpper(plan[:1]) + plan[1:]
}

func description(plan, tierName string) string {
	if plan == DonationPlan {
		// No tier, no period: a donation buys nothing and lasts no time.
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
