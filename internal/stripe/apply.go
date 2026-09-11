package stripe

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/billing"
	"github.com/meshsat/meshsat-hub/internal/invoiceninja"
	"github.com/meshsat/meshsat-hub/internal/mail"
	"github.com/meshsat/meshsat-hub/internal/metrics"
	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/rs/xid"
)

// What each event actually does.
//
// The ordering rule throughout, inherited from the receipts work: the money is
// RECORDED before the plan is granted. A receipt records the payment, not the
// grant, so a grant that fails must still leave a customer owed a document.

// tenantFor resolves the tenant an event is about, without guessing.
//
// The Checkout session carries metadata this Hub set, and so does the
// subscription, because subscription_data[metadata] is copied onto it. Events
// that carry only a customer are resolved through the binding written at
// checkout. Nothing falls back to an email address: a payer may use a personal
// address, a company card or a partner's account, and a payment attributed to
// the wrong tenant is worse than one attributed to none.
func (h *Handler) tenantFor(ctx context.Context, meta map[string]string, customerID string) (*store.Tenant, error) {
	if id := strings.TrimSpace(meta["tenant_id"]); id != "" {
		t, err := h.store.GetTenant(ctx, id)
		if err == nil && t != nil {
			return t, nil
		}
	}
	if c := strings.TrimSpace(customerID); c != "" {
		t, err := h.store.TenantByStripeCustomer(ctx, c)
		if err == nil && t != nil {
			return t, nil
		}
	}
	return nil, ErrNoTenant
}

// once reports whether this call is the one that gets to apply the event.
// Stripe redelivers until it receives a 2xx, so this is a normal path.
func (h *Handler) once(ctx context.Context, eventID, tenantID string) (bool, error) {
	applied, err := h.store.ApplyStripeEvent(ctx, eventID, tenantID)
	if err != nil {
		return false, fmt.Errorf("recording the event: %w", err)
	}
	if !applied {
		slog.Debug("stripe: a redelivered event was already applied", "event", eventID)
	}
	return applied, nil
}

// onCheckout binds the Stripe customer to the tenant and, for a one-off
// payment, records the receipt.
//
// The country collected here is the field Ko-fi never sent. Without it every
// donation from somebody with no account parked in the blocked list; with it,
// internal/vat can decide, and most donations issue on their own.
func (h *Handler) onCheckout(ctx context.Context, ev Event) error {
	var cs checkoutSession
	if err := json.Unmarshal(ev.Data.Object, &cs); err != nil {
		return fmt.Errorf("checkout session: %w", err)
	}
	t, err := h.tenantFor(ctx, cs.Metadata, cs.Customer)
	if err != nil {
		// An anonymous donation is not a payment that lost its tenant. Nobody
		// signed in to make it and there is no account it could belong to, so
		// treating it as unattributable would leave real income with no
		// document, sitting in a list for a person who cannot resolve it.
		//
		// The two are only distinguishable because the Hub stamps every
		// session it creates -- see MetadataKind. This check MUST stay narrow:
		// a session with no tenant and no marker is still genuinely
		// unaccounted for and must keep landing in the unattributed list.
		if isAnonymousDonation(cs) {
			return h.anonymousDonation(ctx, ev, cs)
		}
		h.recordUnattributed(ctx, ev, cs.AmountTotal, cs.Currency, cs.email(), "checkout completed with no tenant")
		return err
	}
	ok, err := h.once(ctx, ev.ID, t.ID)
	if err != nil || !ok {
		return err
	}

	changed := false
	if cs.Customer != "" && t.StripeCustomerID != cs.Customer {
		t.StripeCustomerID = cs.Customer
		changed = true
	}
	// Only fill a country in, never overwrite one. The tenant's own declared
	// country is evidence gathered at enrolment; a card's billing address is
	// weaker, and a customer who pays from an address they are travelling at
	// must not silently change the VAT on their next document.
	if t.BillingCountry == "" && cs.country() != "" {
		t.BillingCountry = cs.country()
		t.BillingCountryEvidence = "billing address at Stripe checkout " + cs.ID
		changed = true
	}
	if changed {
		if err := h.store.UpdateTenant(ctx, t); err != nil {
			return fmt.Errorf("binding the customer: %w", err)
		}
	}

	// A one-off payment is a donation: it gets a document and buys no tier.
	if isOneOffPayment(cs) {
		h.recordReceipt(ctx, t, receiptFacts{
			key:      donationKey(cs.PaymentIntent, cs.ID),
			txn:      cs.PaymentIntent,
			cents:    cs.AmountTotal,
			currency: cs.Currency,
			email:    cs.email(),
			name:     cs.CustomerDetails.Name,
			plan:     billing.DonationPlan,
			tier:     "Donation",
			country:  firstNonEmpty(t.BillingCountry, cs.country()),
			paidAt:   time.Unix(ev.Created, 0).UTC(),
		})
	}
	return nil
}

// isOneOffPayment reports whether this session is a payment rather than a
// subscription, for an amount worth documenting.
//
// It deliberately does NOT require the donation marker. A one-off payment that
// already resolves to a tenant is unambiguous whatever its metadata says, and
// requiring the marker would mean a session created before the marker existed
// produced no receipt if it were paid after the deploy -- money taken with no
// document, which is the one outcome this whole outbox exists to prevent.
func isOneOffPayment(cs checkoutSession) bool {
	return cs.Mode == "payment" && cs.AmountTotal > 0
}

// isAnonymousDonation is the stricter test, used only where there is no tenant.
// Here the marker is the ONLY thing separating a gift from somebody with no
// account from a payment that lost the tenant it should have had, so it is
// required and a session without it stays unattributed.
func isAnonymousDonation(cs checkoutSession) bool {
	return isOneOffPayment(cs) && cs.Metadata[MetadataKind] == KindDonation
}

// anonymousDonation records a gift from somebody with no account.
//
// It hangs off the platform tenant because a receipt row is tenant-scoped --
// an export carries it and a purge destroys it, like every other table with
// that column -- and there is no other tenant it could belong to. Everything
// identifying the giver comes from what Stripe collected at checkout, which is
// also what the document is made out to.
//
// No plan is granted here, and none may ever be. A donation that unlocked
// anything would acquire a counter-performance and stop being outside the
// scope of BTW, which is the whole basis on which it is invoiced without one
// (internal/vat.ForDonation).
func (h *Handler) anonymousDonation(ctx context.Context, ev Event, cs checkoutSession) error {
	t := &store.Tenant{ID: store.DefaultTenantID}
	ok, err := h.once(ctx, ev.ID, t.ID)
	if err != nil || !ok {
		return err
	}
	slog.Info("stripe: donation from somebody with no account",
		"session", cs.ID, "amount_cents", cs.AmountTotal, "currency", cs.Currency,
		"country", cs.country())
	h.recordReceipt(ctx, t, receiptFacts{
		key:      donationKey(cs.PaymentIntent, cs.ID),
		txn:      cs.PaymentIntent,
		cents:    cs.AmountTotal,
		currency: cs.Currency,
		email:    cs.email(),
		name:     cs.CustomerDetails.Name,
		plan:     billing.DonationPlan,
		tier:     "Donation",
		country:  cs.country(),
		paidAt:   time.Unix(ev.Created, 0).UTC(),
	})
	return nil
}

// onSubscription is where a plan actually starts and ends.
//
// customer.subscription.deleted is the event Ko-fi never sent, and having it is
// why the lapse job is a safety net now rather than the mechanism. A lapse still
// suspends nothing and deletes nothing: it lowers the ceiling on REGISTERING
// another device, and every device already registered keeps reporting, keeps its
// dead man's switch and keeps its SOS path.
func (h *Handler) onSubscription(ctx context.Context, ev Event) error {
	var sub subscription
	if err := json.Unmarshal(ev.Data.Object, &sub); err != nil {
		return fmt.Errorf("subscription: %w", err)
	}
	t, err := h.tenantFor(ctx, sub.Metadata, sub.Customer)
	if err != nil {
		h.recordUnattributed(ctx, ev, 0, "", "", "subscription event with no tenant")
		return err
	}
	ok, err := h.once(ctx, ev.ID, t.ID)
	if err != nil || !ok {
		return err
	}

	ending := ev.Type == EventSubscriptionDeleted || !sub.live()
	if ending {
		return h.endPlan(ctx, t, sub, ev)
	}

	plan := h.planFor(sub.priceID())
	if plan == "" {
		// A price nobody mapped. Never guess: record it and leave the plan
		// alone, so somebody decides what was sold.
		slog.Error("stripe: a subscription names a price this Hub does not know; "+
			"the plan is unchanged and this needs a person",
			"tenant", t.ID, "price", sub.priceID(), "subscription", sub.ID)
		h.recordUnattributed(ctx, ev, 0, "", "", "unknown price "+sub.priceID())
		return nil
	}

	expires := sub.periodEnd()
	if expires.IsZero() {
		expires = h.now().UTC().Add(billing.Period)
	}
	expires = expires.Add(grace)

	was := t.Plan
	t.Plan, t.PlanExpiresAt, t.StripeSubscriptionID = plan, &expires, sub.ID
	if sub.Customer != "" {
		t.StripeCustomerID = sub.Customer
	}
	// A new period means the previous lapse warning is spent.
	t.LapseWarnedAt = nil
	if err := h.store.UpdateTenant(ctx, t); err != nil {
		return fmt.Errorf("granting the plan: %w", err)
	}
	h.announce(t.ID)
	slog.Info("stripe: plan granted", "tenant", t.ID, "plan", plan, "was", was,
		"expires", expires.Format(time.RFC3339), "subscription", sub.ID)
	h.log(ctx, t.ID, "subscription_granted", fmt.Sprintf(
		"plan %s until %s from subscription %s", plan, expires.Format(time.RFC3339), sub.ID))

	if was != plan {
		h.notifyPlanChanged(ctx, t, plan, expires)
	}
	return nil
}

// endPlan drops a tenant to free. It is deliberately the same shape as the
// lapse job's ending, because a cancellation and a lapse mean the same thing to
// a customer and must not behave differently.
func (h *Handler) endPlan(ctx context.Context, t *store.Tenant, sub subscription, ev Event) error {
	if t.PlanExpiresAt == nil {
		// No expiry means an operator set this tier by hand. A cancelled
		// subscription must not silently convert that into an ended plan.
		slog.Info("stripe: subscription ended for a tenant on an operator-set tier; leaving it alone",
			"tenant", t.ID, "plan", t.Plan)
		return nil
	}
	was := t.Plan
	t.Plan, t.PlanExpiresAt, t.StripeSubscriptionID = plans.Free, nil, ""
	if err := h.store.UpdateTenant(ctx, t); err != nil {
		return fmt.Errorf("ending the plan: %w", err)
	}
	h.announce(t.ID)
	slog.Info("stripe: plan ended", "tenant", t.ID, "was", was, "status", sub.Status,
		"subscription", sub.ID)
	h.log(ctx, t.ID, "subscription_ended", fmt.Sprintf(
		"%s ended; subscription %s is %s", was, sub.ID, sub.Status))
	h.notifyLapsed(ctx, t, was)
	return nil
}

// onInvoicePaid is the document trigger. An invoice is paid once per period,
// which is exactly when a receipt is owed.
func (h *Handler) onInvoicePaid(ctx context.Context, ev Event) error {
	var inv invoice
	if err := json.Unmarshal(ev.Data.Object, &inv); err != nil {
		return fmt.Errorf("invoice: %w", err)
	}
	if inv.AmountPaid <= 0 {
		// A zero invoice is a proration or a trial. Nothing was paid, so
		// nothing is owed.
		return nil
	}
	// The subscription's own metadata comes with the invoice, so this does not
	// depend on the customer having been bound yet. It must not: Stripe sends
	// invoice.paid BEFORE checkout.session.completed on a first subscription,
	// and the binding is written by the latter. Reading only the customer left
	// the first real payment unattributed and undocumented.
	t, err := h.tenantFor(ctx, inv.metadata(), inv.Customer)
	if err != nil {
		h.recordUnattributed(ctx, ev, inv.AmountPaid, inv.Currency, inv.CustomerEmail, "invoice paid with no tenant")
		return err
	}
	ok, err := h.once(ctx, ev.ID, t.ID)
	if err != nil || !ok {
		return err
	}
	h.recordReceipt(ctx, t, receiptFacts{
		key:      invoiceKey(inv.ID),
		txn:      inv.ID,
		cents:    inv.AmountPaid,
		currency: inv.Currency,
		email:    inv.CustomerEmail,
		name:     inv.CustomerName,
		plan:     firstNonEmpty(h.planFor(inv.priceID()), t.Plan),
		tier:     tierName(h.planFor(inv.priceID())),
		country:  firstNonEmpty(t.BillingCountry, inv.country()),
		paidAt:   inv.paidAt(),
	})
	return nil
}

// onChargeRefunded turns a refund made in Stripe into a credit note, with no
// operator involved.
//
// Ko-fi sent nothing for a refund, so POST /api/admin/receipts/{id}/refund
// existed for a person to record by hand what had already happened. That
// endpoint stays, because a refund can still be made outside Stripe, but it is
// no longer the only way a customer gets their credit note.
// onInvoiceFailed records a renewal the provider could not take.
//
// It changes NO plan, and that is the point rather than an omission. A failed
// card is Stripe retrying over the following days, not a cancellation, and
// onSubscription already keeps past_due and unpaid on their tier for exactly
// that reason. Ending somebody's plan because one attempt failed would take a
// fleet off the air over an expired card.
//
// What it does is make the attempt visible, which nothing did before: six
// events were handled and none of them was a failure, so a declined renewal --
// or one blocked by Radar, which this account now runs in a stricter mode --
// left no metric, no audit entry and no word to the customer. The first anyone
// would know is a plan quietly lapsing weeks later.
func (h *Handler) onInvoiceFailed(ctx context.Context, ev Event) error {
	var inv invoice
	if err := json.Unmarshal(ev.Data.Object, &inv); err != nil {
		return fmt.Errorf("invoice: %w", err)
	}
	if inv.AmountDue <= 0 {
		// Nothing was owed, so nothing failed in a way anybody cares about.
		return nil
	}
	metrics.PaymentsFailedTotal.Inc()

	// Same as onInvoicePaid: the tenant rides on the subscription's metadata,
	// so a failure is visible even for a customer not yet bound.
	t, err := h.tenantFor(ctx, inv.metadata(), inv.Customer)
	if err != nil {
		h.recordUnattributed(ctx, ev, inv.AmountDue, inv.Currency, inv.CustomerEmail, "invoice payment failed with no tenant")
		return err
	}
	ok, err := h.once(ctx, ev.ID, t.ID)
	if err != nil || !ok {
		return err
	}

	// Stripe stops retrying eventually. Until then this is a warning; after
	// it, it is the final answer on the payment and the plan will lapse on its
	// own date.
	final := inv.NextPaymentAttempt == 0
	detail, _ := json.Marshal(map[string]any{
		"provider": "stripe", "event": ev.ID, "invoice": inv.ID,
		"amount_cents": inv.AmountDue, "currency": inv.Currency,
		"attempt": inv.AttemptCount, "final": final,
	})
	h.log(ctx, t.ID, "payment_failed", string(detail))
	slog.Warn("stripe: a renewal could not be taken",
		"tenant", t.ID, "invoice", inv.ID, "amount_cents", inv.AmountDue,
		"currency", inv.Currency, "attempt", inv.AttemptCount, "final", final,
		"plan", t.Plan, "note", "the plan is unchanged; a failing card is a retry, not a cancellation")

	h.notifyPaymentFailed(ctx, t, inv, final)
	return nil
}

func (h *Handler) notifyPaymentFailed(ctx context.Context, t *store.Tenant, inv invoice, final bool) {
	if h.mail == nil {
		return
	}
	to := firstNonEmpty(inv.CustomerEmail, h.ownerEmail(ctx, t))
	if to == "" {
		return
	}
	var retryAt time.Time
	if !final {
		retryAt = time.Unix(inv.NextPaymentAttempt, 0).UTC()
	}
	// Written the way the customer's own documents write it: internal/refunds
	// does the same, so a figure in this mail matches the one on their invoice
	// rather than being a second dialect of the same amount.
	amount := invoiceninja.FormatMoney(inv.AmountDue, inv.Currency, t.BillingCountry)
	// A nil expiry is an operator-set tier, which has no end date to quote.
	var ends time.Time
	if t.PlanExpiresAt != nil {
		ends = t.PlanExpiresAt.UTC()
	}
	msg := mail.PaymentFailed(h.ownerName(ctx, t), t.Plan, amount, retryAt, ends, h.hubURL)
	mail.SendOrLog(ctx, h.mail, to, msg, "payment failed")
}

func (h *Handler) onChargeRefunded(ctx context.Context, ev Event) error {
	var ch charge
	if err := json.Unmarshal(ev.Data.Object, &ch); err != nil {
		return fmt.Errorf("charge: %w", err)
	}
	if h.refunds == nil || h.receipts == nil || ch.AmountRefunded <= 0 {
		return nil
	}
	// Find the payment this reverses, by the same key its receipt was filed
	// under. An invoice for a subscription, a payment intent for a donation.
	var rec *store.Receipt
	for _, key := range []string{invoiceKey(ch.Invoice), donationKey(ch.PaymentIntent, "")} {
		if key == "" {
			continue
		}
		got, err := h.receipts.GetReceiptByKey(ctx, key)
		if err == nil && got != nil {
			rec = got
			break
		}
	}
	if rec == nil {
		// The money went back and this Hub has no document to reverse. Say so
		// loudly: it is recoverable by hand and invisible otherwise.
		slog.Error("stripe: a charge was refunded but no receipt matches it; "+
			"the credit note has to be raised by hand",
			"charge", ch.ID, "invoice", ch.Invoice, "amount_refunded", ch.AmountRefunded)
		h.recordUnattributed(ctx, ev, ch.AmountRefunded, ch.Currency, "", "refund with no matching receipt")
		return nil
	}
	ok, err := h.once(ctx, ev.ID, rec.TenantID)
	if err != nil || !ok {
		return err
	}
	r := &store.Refund{
		ID:          xid.New().String(),
		TenantID:    rec.TenantID,
		ReceiptID:   rec.ID,
		AmountCents: ch.AmountRefunded,
		Currency:    strings.ToUpper(rec.Currency),
		// Frozen from the receipt, never the tenant's current country: a
		// customer who moves must not change the VAT on a document already
		// issued.
		Country:     rec.Country,
		Reason:      "refunded in Stripe (charge " + ch.ID + ")",
		RequestedBy: "stripe",
		RefundedAt:  h.now().UTC(),
		Status:      store.RefundPending,
	}
	created, err := h.refunds.CreateRefund(ctx, r)
	if err != nil {
		return fmt.Errorf("recording the refund: %w", err)
	}
	if !created {
		// receipt_id is UNIQUE: a payment is refunded once. A second partial
		// refund of the same charge arrives here and is left for a person,
		// because changing an issued credit note is not something to do
		// automatically.
		slog.Warn("stripe: this payment already has a refund recorded; the new figure needs a person",
			"receipt", rec.ID, "charge", ch.ID, "amount_refunded", ch.AmountRefunded)
		return nil
	}
	slog.Info("stripe: refund recorded from the payment processor", "refund", r.ID,
		"tenant", rec.TenantID, "receipt", rec.ID, "amount_cents", r.AmountCents)
	h.log(ctx, rec.TenantID, "refund_recorded", fmt.Sprintf(
		"%d %s refunded in Stripe against receipt %s", r.AmountCents, r.Currency, rec.ID))
	return nil
}

// --- shared helpers -------------------------------------------------------

// invoiceKey and donationKey are the idempotency tokens a receipt is filed
// under. They are derived from Stripe's own stable object ids rather than from
// the event id, so the refund path can compute the same key later without
// having kept the event.
func invoiceKey(invoiceID string) string {
	if strings.TrimSpace(invoiceID) == "" {
		return ""
	}
	return "stripe:inv:" + invoiceID
}

func donationKey(paymentIntent, sessionID string) string {
	if pi := strings.TrimSpace(paymentIntent); pi != "" {
		return "stripe:pi:" + pi
	}
	if s := strings.TrimSpace(sessionID); s != "" {
		return "stripe:cs:" + s
	}
	return ""
}

type receiptFacts struct {
	key, txn, currency, email, name, plan, tier, country string
	cents                                                int64
	paidAt                                               time.Time
}

// recordReceipt writes one row for the outbox in internal/billing to drain. A
// failure is logged, never returned: the money arrived either way, and making
// Stripe retry the whole event because a document could not be filed would
// replay the payment.
func (h *Handler) recordReceipt(ctx context.Context, t *store.Tenant, f receiptFacts) {
	if h.receipts == nil || f.key == "" || f.cents <= 0 {
		return
	}
	email := strings.TrimSpace(f.email)
	if email == "" {
		email = h.ownerEmail(ctx, t)
	}
	r := &store.Receipt{
		ID:            xid.New().String(),
		TenantID:      t.ID,
		DeliveryKey:   f.key,
		TransactionID: f.txn,
		Country:       f.country,
		Email:         email,
		Name:          f.name,
		AmountCents:   f.cents,
		Currency:      strings.ToUpper(f.currency),
		Plan:          f.plan,
		TierName:      f.tier,
		PaidAt:        f.paidAt,
		Status:        store.ReceiptPending,
		NextAttemptAt: h.now().UTC(),
	}
	created, err := h.receipts.CreateReceipt(ctx, r)
	if err != nil {
		slog.Error("stripe: could not record the receipt; the payment stands and this needs a person",
			"tenant", t.ID, "key", f.key, "error", err)
		return
	}
	if !created {
		slog.Debug("stripe: a receipt for this payment already exists", "key", f.key)
		return
	}
	slog.Info("stripe: payment recorded for a receipt", "tenant", t.ID,
		"receipt", r.ID, "amount_cents", r.AmountCents, "currency", r.Currency)
}

// recordUnattributed keeps money that could not be placed visible to a person:
// an audit entry on the platform tenant, listed at
// GET /api/admin/payments/unmatched.
func (h *Handler) recordUnattributed(ctx context.Context, ev Event, cents int64, currency, email, why string) {
	metrics.PaymentsUnattributedTotal.Inc()
	detail, _ := json.Marshal(map[string]any{
		"provider":     "stripe",
		"event":        ev.ID,
		"type":         ev.Type,
		"amount_cents": cents,
		"currency":     currency,
		"payer_email":  email,
		"reason":       why,
	})
	slog.Warn("stripe: an event could not be attributed to a tenant",
		"event", ev.ID, "type", ev.Type, "reason", why)
	h.log(ctx, store.DefaultTenantID, "payment_unattributed", string(detail))
}

func (h *Handler) log(ctx context.Context, tenantID, action, detail string) {
	if h.audit == nil {
		return
	}
	if err := h.audit.Log(ctx, tenantID, action, "stripe_webhook", detail, ""); err != nil {
		slog.Warn("stripe: could not write the audit entry", "action", action, "error", err)
	}
}

// announce tells the other replicas a plan changed, so the ceiling applies
// everywhere at once rather than at the end of somebody else's cache TTL.
func (h *Handler) announce(tenantID string) {
	if h.forget != nil {
		h.forget(tenantID)
	}
}

func (h *Handler) ownerEmail(ctx context.Context, t *store.Tenant) string {
	if h.users == nil || t.OwnerUserID == "" {
		return ""
	}
	u, err := h.users.GetUserByID(ctx, t.ID, t.OwnerUserID)
	if err != nil || u == nil {
		return ""
	}
	return u.Email
}

func (h *Handler) ownerName(ctx context.Context, t *store.Tenant) string {
	if h.users == nil || t.OwnerUserID == "" {
		return ""
	}
	u, err := h.users.GetUserByID(ctx, t.ID, t.OwnerUserID)
	if err != nil || u == nil {
		return ""
	}
	return u.Name
}

func (h *Handler) notifyPlanChanged(ctx context.Context, t *store.Tenant, plan string, expires time.Time) {
	if h.mail == nil {
		return
	}
	to := h.ownerEmail(ctx, t)
	if to == "" {
		return
	}
	msg := mail.PlanChanged(h.ownerName(ctx, t), plan, plans.For(plan).Devices, expires, h.hubURL)
	mail.SendOrLog(ctx, h.mail, to, msg, "plan changed")
}

func (h *Handler) notifyLapsed(ctx context.Context, t *store.Tenant, was string) {
	if h.mail == nil {
		return
	}
	to := h.ownerEmail(ctx, t)
	if to == "" {
		return
	}
	msg := mail.Lapsed(h.ownerName(ctx, t), was, h.now().UTC(), h.hubURL)
	mail.SendOrLog(ctx, h.mail, to, msg, "subscription ended")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// tierName is what the customer sees on their document.
func tierName(plan string) string {
	switch plan {
	case plans.Crew:
		return "Crew"
	case plans.Fleet:
		return "Fleet"
	}
	return ""
}
