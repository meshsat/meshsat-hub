package stripe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/meshsat/meshsat-hub/internal/billing"
	"github.com/meshsat/meshsat-hub/internal/mail"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// Handler serves POST /api/webhook/stripe/{secret}.
//
// One Stripe account serves every tenant, so this is platform-level rather than
// per-tenant: the payment says which tenant it is for, through metadata this Hub
// put on the Checkout session itself. That is the whole reason the claim code is
// gone -- a payment is bound to a tenant by construction instead of by a
// customer remembering to type something.
type Handler struct {
	store    TenantStore
	receipts billing.ReceiptStore
	refunds  RefundStore
	secret   string
	prices   tierMap
	audit    Auditor
	mail     mail.Sender
	users    UserLookup
	hubURL   string
	forget   func(tenantID string)
	now      Clock
}

// RefundStore is what turning charge.refunded into a credit note needs. The
// document itself is produced by internal/refunds, which this package neither
// imports nor knows about: it writes a row and the drainer does the rest.
type RefundStore interface {
	CreateRefund(ctx context.Context, r *store.Refund) (bool, error)
}

// UserLookup turns a tenant's owner into an account. Tenant.OwnerUserID holds a
// user id, never an address.
type UserLookup interface {
	GetUserByID(ctx context.Context, tenantID, id string) (*store.LocalUser, error)
}

// grace is how long past a subscription's period end a plan keeps its ceiling.
//
// The expiry is a safety net, not the mechanism: customer.subscription.deleted
// is what actually ends a plan, immediately. This exists so that a renewal
// webhook lost to an outage costs nobody their fleet ceiling while Stripe is
// still happily charging their card.
const grace = 3 * 24 * time.Hour

// maxBody bounds a delivery. Stripe's events are small; anything near this is
// not one.
const maxBody = 1 << 20

// NewHandler creates the webhook handler. With no signing secret it refuses
// every delivery, which is the right state for a payment endpoint nobody has
// configured.
func NewHandler(s TenantStore, signingSecret string) *Handler {
	return &Handler{store: s, secret: signingSecret, prices: tierMap{}, now: time.Now}
}

func (h *Handler) SetAudit(a Auditor)                     { h.audit = a }
func (h *Handler) SetReceipts(r billing.ReceiptStore)     { h.receipts = r }
func (h *Handler) SetRefunds(r RefundStore)               { h.refunds = r }
func (h *Handler) SetUserLookup(u UserLookup)             { h.users = u }
func (h *Handler) SetInvalidator(f func(string))          { h.forget = f }
func (h *Handler) SetMailer(m mail.Sender, hubURL string) { h.mail, h.hubURL = m, hubURL }

// ServeHTTP handles a Stripe webhook delivery.
//
//	@Summary      Stripe payment webhook
//	@Description  Applies subscription, invoice and refund events. The path secret identifies the endpoint; the Stripe-Signature header authenticates the caller and is the only thing trusted.
//	@Tags         webhooks
//	@Accept       json
//	@Produce      plain
//	@Success      200  {string}  string  "applied, or acknowledged and ignored"
//	@Failure      400  {string}  string  "the signature does not verify"
//	@Failure      500  {string}  string  "a store failure; Stripe will retry"
//	@Failure      503  {string}  string  "billing is not configured"
//	@Router       /api/webhook/stripe/{secret} [post]
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.secret == "" {
		http.Error(w, "billing is not configured", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	// The RAW body, before any decoding. Re-encoding a parsed body changes the
	// bytes and the signature is over bytes.
	if err := VerifySignature(r.Header.Get("Stripe-Signature"), body, h.secret, h.now()); err != nil {
		// Never say which part failed: that turns the endpoint into an oracle.
		slog.Warn("stripe: refused a delivery", "error", err, "remote", r.RemoteAddr)
		http.Error(w, "signature does not verify", http.StatusBadRequest)
		return
	}

	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil || ev.ID == "" {
		slog.Warn("stripe: a verified delivery did not parse", "error", err)
		http.Error(w, "unreadable event", http.StatusBadRequest)
		return
	}

	// A store failure is a 500 so Stripe retries; anything else is a 2xx so it
	// stops. An event we do not act on is not a failure.
	if err := h.dispatch(r.Context(), ev); err != nil {
		if errors.Is(err, ErrNoTenant) {
			// Recorded for a person, not retried: redelivering will not make a
			// tenant appear.
			writeText(w, http.StatusOK, "recorded")
			return
		}
		slog.Error("stripe: could not apply an event", "event", ev.ID, "type", ev.Type, "error", err)
		http.Error(w, "could not apply", http.StatusInternalServerError)
		return
	}
	writeText(w, http.StatusOK, "ok")
}

func writeText(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, msg)
}

func (h *Handler) dispatch(ctx context.Context, ev Event) error {
	switch ev.Type {
	case EventCheckoutCompleted:
		return h.onCheckout(ctx, ev)
	case EventSubscriptionCreated, EventSubscriptionUpdated, EventSubscriptionDeleted:
		return h.onSubscription(ctx, ev)
	case EventInvoicePaid:
		return h.onInvoicePaid(ctx, ev)
	case EventInvoiceFailed:
		return h.onInvoiceFailed(ctx, ev)
	case EventChargeRefunded:
		return h.onChargeRefunded(ctx, ev)
	default:
		// Acknowledged and ignored. Answering anything else would have Stripe
		// retry an event this Hub has no opinion about for three days.
		slog.Debug("stripe: ignoring an event type this Hub does not act on", "type", ev.Type)
		return nil
	}
}
