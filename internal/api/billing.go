package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/stripe"
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

// SetDonationPrice takes the price with a customer-chosen amount. Empty means
// there is no donation path, which is a supported state: both donation
// endpoints then answer 503 rather than 500.
func (h *BillingHandler) SetDonationPrice(priceID string) { h.donatio = strings.TrimSpace(priceID) }

type donateRequest struct {
	// Email is only a suggestion for the checkout form and may be empty. The
	// address Stripe collects is the one that reaches the document.
	Email string `json:"email"`
}

// donate is the half both donation endpoints share. tenantID is empty for a
// giver with no account, which is allowed: stripe.DonationRequest takes an
// optional tenant, and the webhook tells an anonymous gift apart from a
// misrouted payment by the marker the client stamps on the session.
func (h *BillingHandler) donate(w http.ResponseWriter, r *http.Request, tenantID, email string) {
	if h.donatio == "" {
		writeError(w, http.StatusServiceUnavailable, "donations are not configured")
		return
	}
	if h.client == nil {
		writeError(w, http.StatusServiceUnavailable, "billing is not configured")
		return
	}
	s, err := h.client.Donation(r.Context(), stripe.DonationRequest{
		TenantID:   tenantID,
		Email:      email,
		PriceID:    h.donatio,
		SuccessURL: h.hubURL + "/#/settings?donation=thanks",
		CancelURL:  h.hubURL + "/#/settings?donation=cancelled",
	})
	if err != nil {
		slog.Error("billing: could not start a donation", "tenant", tenantID, "error", err)
		writeError(w, http.StatusBadGateway, "the payment provider could not be reached")
		return
	}
	slog.Info("billing: donation started", "tenant", tenantID, "session", s.ID)
	writeJSON(w, http.StatusOK, checkoutResponse{URL: s.URL})
}

// DonateRedirect starts an anonymous donation and sends the browser straight
// to Stripe.
//
// It exists because meshsat.net cannot reach the JSON endpoint. The site's CSP
// allows connect-src only to itself, its analytics host and api.github.com,
// and form-action only to 'self', so a fetch() or a form POST from the site to
// this Hub is blocked by the browser. A plain link is the one thing that needs
// no CSP change, no CORS, and no JavaScript on a static site -- so the button
// is an anchor and this is where it lands.
//
// A GET with an effect is a deliberate trade. The effect is small and local:
// a Checkout session is created at Stripe, nothing is written here, no money
// moves, and an unused session expires by itself. It is the same shape as a
// Stripe Payment Link, which is a URL people put in a page. The route is rate
// limited per IP, and the anchor carries rel="nofollow" so crawlers leave it
// alone.
//
// @Summary      Donate without an account
// @Description  Creates a Stripe Checkout session for an anonymous donation and redirects to it. For linking from a static page.
// @Tags         billing
// @Success      303  "redirect to Stripe Checkout"
// @Failure      503  {string}  string  "donations are not available"
// @Router       /donate [get]
func (h *BillingHandler) DonateRedirect(w http.ResponseWriter, r *http.Request) {
	if h.donatio == "" || h.client == nil {
		// A person followed a link, so answer in words rather than JSON.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("Donations are not available at the moment. Nothing was charged.\n"))
		return
	}
	s, err := h.client.Donation(r.Context(), stripe.DonationRequest{
		PriceID:    h.donatio,
		SuccessURL: h.hubURL + "/#/?donation=thanks",
		CancelURL:  h.hubURL + "/#/?donation=cancelled",
	})
	if err != nil {
		slog.Error("billing: could not start an anonymous donation", "error", err)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("The payment provider could not be reached. Nothing was charged.\n"))
		return
	}
	slog.Info("billing: anonymous donation started", "session", s.ID)
	// 303, not 302: the browser must GET the Stripe page, and a later back
	// button must not silently repeat this.
	http.Redirect(w, r, s.URL, http.StatusSeeOther)
}

// Donate starts a one-off donation for a signed-in tenant.
// @Summary      Donate
// @Description  Creates a Stripe Checkout session for a one-off donation bound to this tenant. Grants no plan.
// @Tags         billing
// @Produce      json
// @Success      200  {object}  checkoutResponse
// @Failure      403  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/tenant/billing/donate [post]
func (h *BillingHandler) Donate(w http.ResponseWriter, r *http.Request) {
	t := h.tenant(r)
	if t == nil {
		writeError(w, http.StatusForbidden, "no tenant")
		return
	}
	h.donate(w, r, t.ID, h.ownerEmail(r.Context(), t))
}

// DonatePublic starts a one-off donation from somebody with no account.
// @Summary      Donate without an account
// @Description  Creates a Stripe Checkout session for a one-off donation with no tenant. Unauthenticated and rate limited. Grants no plan.
// @Tags         billing
// @Accept       json
// @Produce      json
// @Success      200  {object}  checkoutResponse
// @Failure      400  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/donate [post]
func (h *BillingHandler) DonatePublic(w http.ResponseWriter, r *http.Request) {
	// No tenant, and deliberately no attempt to find one. This route is exempt
	// from the auth chain, so TenantMiddleware never ran and there is nothing
	// in the context to read -- calling h.tenant(r) here would always return
	// nil and invite somebody to "fix" it later by trusting a header.
	var req donateRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := readJSON(w, r, &req, 1024); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	h.donate(w, r, "", strings.TrimSpace(req.Email))
}

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
	// The request is validated BEFORE the backend is checked. A plan that
	// cannot be bought is a client error whether or not the payment provider
	// happens to be configured, and answering 503 to it would send somebody
	// looking at the wrong thing.
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
	if h.client == nil {
		writeError(w, http.StatusServiceUnavailable, "billing is not configured")
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
		PlanName:   plan,
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
	// hubauth, NOT tenancy: the HTTP auth middleware puts the tenant under
	// auth.TenantContextKey, while tenancy.FromContext reads the key the MQTT
	// and webhook paths use. Getting this wrong 403s every signed-in customer
	// who presses Subscribe, and it type-checks perfectly.
	id := hubauth.TenantIDFromContext(r.Context())
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
