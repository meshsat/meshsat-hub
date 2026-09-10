package kofi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/invoiceninja"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// The Ko-fi half of the receipt path: everything that has to read Ko-fi's own
// payload shape. The document itself is issued by internal/billing, which never
// sees a Payload.

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
// parkUnreadable records a payment whose amount could not be parsed, blocked,
// so that money taken always leaves a document trail somebody can find. The
// amount is deliberately zero: the figure is exactly what could not be read,
// and an operator reads the real one off the transaction reference.
func (h *Handler) parkUnreadable(ctx context.Context, t *store.Tenant, p Payload, plan string, cause error) {
	r := &store.Receipt{
		TenantID:      t.ID,
		DeliveryKey:   deliveryKey(p),
		TransactionID: p.KofiTransactionID,
		Country:       t.BillingCountry,
		Email:         h.receiptEmail(ctx, t, p),
		Name:          t.Name,
		Currency:      strings.ToUpper(strings.TrimSpace(p.Currency)),
		Plan:          plan,
		TierName:      p.TierName,
		PaidAt:        paidAt(p),
		Status:        store.ReceiptBlocked,
		LastError:     "the amount " + p.Amount + " could not be read: " + cause.Error(),
	}
	if _, err := h.receipts.CreateReceipt(ctx, r); err != nil {
		slog.Error("kofi: could not even park an unreadable payment",
			"tenant", t.ID, "txn", p.KofiTransactionID, "error", err)
	}
}

func (h *Handler) recordReceipt(ctx context.Context, t *store.Tenant, p Payload, plan string) {
	if h.receipts == nil {
		return
	}
	key := deliveryKey(p)
	cents, err := invoiceninja.ParseAmount(p.Amount)
	if err != nil {
		// Park it rather than return. Money has been taken and the customer is
		// owed a document; returning here left no row at all, so the outbox had
		// nothing to retry and the blocked list showed nothing. The only trace
		// was this log line, which nothing alerts on (MESHSAT-1016).
		slog.Error("kofi: payment amount could not be read; parking the receipt for a person",
			"tenant", t.ID, "txn", p.KofiTransactionID, "error", err)
		h.parkUnreadable(ctx, t, p, plan, err)
		return
	}

	r := &store.Receipt{
		TenantID:      t.ID,
		DeliveryKey:   key,
		TransactionID: p.KofiTransactionID,
		Country:       t.BillingCountry,
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
