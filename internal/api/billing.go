package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/stripe"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// Starting and managing a subscription (MESHSAT-1023).
//
// Checkout begins HERE rather than at a link the customer is handed, and that
// is the whole difference from what came before. A session created by this
// handler carries the tenant id in its metadata, so the payment is bound to a
// tenant by construction. The predecessor had no such field: it asked the
// customer to type an eight-character code into a message box, and every
// payment where they forgot ended up in a list for a person to place by hand.

// BillingHandler serves the tenant's own billing actions.
type BillingHandler struct {
	store   store.Store
	client  *stripe.Client
	prices  map[string]string // plan -> price id
	hubURL  string
	donatio string // the donation price id, empty when there is no donation path
}

// NewBillingHandler returns a handler. A nil client is a supported state: the
// endpoints then report that billing is not configured rather than 500.
func NewBillingHandler(s store.Store, c *stripe.Client, hubURL string) *BillingHandler {
	return &BillingHandler{store: s, client: c, prices: map[string]string{}, hubURL: strings.TrimRight(hubURL, "/")}
}

// SetPrices takes the price-id-to-plan map from config and inverts it, because
// a customer picks a plan and this has to find its price.
func (h *BillingHandler) SetPrices(priceToPlan map[string]string) {
	h.prices = map[string]string{}
	for price, plan := range priceToPlan {
		h.prices[plans.Normalise(plan)] = price
	}
}

func (h *BillingHandler) SetDonationPrice(priceID string) { h.donatio = strings.TrimSpace(priceID) }

type checkoutRequest struct {
	Plan string `json:"plan"`
}

type checkoutResponse struct {
	URL string `json:"url"`
}

// Checkout starts a subscription for this tenant.
// @Summary      Start a subscription
// @Description  Creates a Stripe Checkout session bound to this tenant and returns the URL to send the customer to.
// @Tags         billing
// @Accept       json
// @Produce      json
// @Param        request body checkoutRequest true "the plan to buy"
// @Success      200  {object}  checkoutResponse
// @Failure      400  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/tenant/billing/checkout [post]
func (h *BillingHandler) Checkout(w http.ResponseWriter, r *http.Request) {
	if h.client == nil {
		writeError(w, http.StatusServiceUnavailable, "billing is not configured")
		return
	}
	var req checkoutRequest
	if err := readJSON(w, r, &req, 4096); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	plan := plans.Normalise(req.Plan)
	price := h.prices[plan]
	if price == "" {
		// Only the sellable tiers have prices. custom and beta are
		// operator-set, and this is where a customer would otherwise try.
		writeError(w, http.StatusBadRequest, "that plan cannot be bought here")
		return
	}
	t := h.tenant(r)
	if t == nil {
		writeError(w, http.StatusForbidden, "no tenant")
		return
	}
	s, err := h.client.Checkout(r.Context(), stripe.CheckoutRequest{
		TenantID:   t.ID,
		Email:      h.ownerEmail(r.Context(), t),
		PriceID:    price,
		CustomerID: t.StripeCustomerID,
		SuccessURL: h.hubURL + "/#/settings?checkout=done",
		CancelURL:  h.hubURL + "/#/settings?checkout=cancelled",
	})
	if err != nil {
		slog.Error("billing: could not start a checkout", "tenant", t.ID, "plan", plan, "error", err)
		writeError(w, http.StatusBadGateway, "the payment provider could not be reached")
		return
	}
	slog.Info("billing: checkout started", "tenant", t.ID, "plan", plan, "session", s.ID)
	writeJSON(w, http.StatusOK, checkoutResponse{URL: s.URL})
}

// Portal opens the provider's own customer portal.
// @Summary      Manage billing
// @Description  Returns a URL to Stripe's customer portal, where the customer can cancel, change card or read past invoices.
// @Tags         billing
// @Produce      json
// @Success      200  {object}  checkoutResponse
// @Failure      404  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/tenant/billing/portal [post]
func (h *BillingHandler) Portal(w http.ResponseWriter, r *http.Request) {
	if h.client == nil {
		writeError(w, http.StatusServiceUnavailable, "billing is not configured")
		return
	}
	t := h.tenant(r)
	if t == nil {
		writeError(w, http.StatusForbidden, "no tenant")
		return
	}
	if t.StripeCustomerID == "" {
		// Nothing has ever been paid for this tenant, so there is nothing to
		// manage. A 404 says that more honestly than an empty portal would.
		writeError(w, http.StatusNotFound, "there is no subscription to manage yet")
		return
	}
	s, err := h.client.Portal(r.Context(), t.StripeCustomerID, h.hubURL+"/#/settings")
	if err != nil {
		slog.Error("billing: could not open the portal", "tenant", t.ID, "error", err)
		writeError(w, http.StatusBadGateway, "the payment provider could not be reached")
		return
	}
	writeJSON(w, http.StatusOK, checkoutResponse{URL: s.URL})
}

func (h *BillingHandler) tenant(r *http.Request) *store.Tenant {
	id := tenancy.FromContext(r.Context())
	if id == "" || h.store == nil {
		return nil
	}
	t, err := h.store.GetTenant(r.Context(), id)
	if err != nil {
		return nil
	}
	return t
}

// ownerEmail is only a suggestion for the checkout form: the customer may
// correct it, and the address Stripe collects is the one that reaches the
// receipt.
func (h *BillingHandler) ownerEmail(ctx context.Context, t *store.Tenant) string {
	if h.store == nil || t.OwnerUserID == "" {
		return ""
	}
	u, err := h.store.GetUserByID(ctx, t.ID, t.OwnerUserID)
	if err != nil || u == nil {
		return ""
	}
	return u.Email
}
