package api

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/quota"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// TenantUsageHandler reports what a tenant has registered against what its
// plan allows (MESHSAT-989). Read-only, and deliberately separate from the
// enforcement path: this endpoint exists so a person can see the number before
// a create fails, and so the Fleet page can grey out a button rather than
// letting someone fill in a form for a 402.
type TenantUsageHandler struct {
	quota *quota.Checker
	store store.Store
}

// NewTenantUsageHandler creates the usage handler.
func NewTenantUsageHandler(q *quota.Checker, s store.Store) *TenantUsageHandler {
	return &TenantUsageHandler{quota: q, store: s}
}

// usageResponse is the wire shape. The tier table travels with it so the UI
// does not carry its own copy of the numbers and drift from the server when a
// tier is re-priced.
type usageResponse struct {
	quota.Usage
	Tiers []tierResponse `json:"tiers"`
	// UpgradeURL is where a person goes to pay, for as long as the old
	// provider is still the one taking money. It is empty once checkout starts
	// inside the Hub, and the UI shows a Subscribe button instead.
	UpgradeURL string `json:"upgrade_url,omitempty"`
	// Billing reports whether this tenant can start a checkout and whether it
	// has anything to manage. The claim code it replaces is gone: a payment is
	// bound to a tenant by the metadata on its Checkout session now, not by
	// somebody typing a code into a message box (MESHSAT-1023).
	Billing billingState `json:"billing"`
}

// billingState is what the Settings page needs to decide which buttons to draw.
type billingState struct {
	// Provider is "stripe" when checkout starts in the Hub, "kofi" while the
	// old link is still the way to pay, and empty when nothing is configured.
	Provider string `json:"provider,omitempty"`
	// Manageable is true once there is a Stripe customer behind this tenant,
	// which is what the portal needs.
	Manageable bool `json:"manageable"`
}

type tierResponse struct {
	Plan    string `json:"plan"`
	Devices int    `json:"devices"` // -1 = no ceiling
	Current bool   `json:"current"`
}

// upgradeURL is where the tier links point. Set once at startup.
var upgradeURL string

// SetUpgradeURL configures the payment link shown beside the tiers.
func SetUpgradeURL(u string) { upgradeURL = u }

// Usage reports this tenant's device count against its plan.
//
//	@Summary      Devices and bridges used against the plan's ceiling
//	@Description  The ceiling applies to registering new devices only. Everything already registered keeps working, keeps reporting, and keeps its SOS path, whatever the plan says.
//	@Tags         tenant
//	@Produce      json
//	@Success      200  {object}  usageResponse
//	@Failure      500  {object}  map[string]string
//	@Router       /api/tenant/usage [get]
func (h *TenantUsageHandler) Usage(w http.ResponseWriter, r *http.Request) {
	h.usage(w, r, hubauth.TenantIDFromContext(r.Context()))
}

// AdminUsage is the platform-admin equivalent, addressed by id.
//
//	@Summary      Devices and bridges used by a tenant
//	@Tags         admin
//	@Produce      json
//	@Param        id   path      string  true  "tenant id"
//	@Success      200  {object}  usageResponse
//	@Failure      404  {object}  map[string]string
//	@Failure      500  {object}  map[string]string
//	@Router       /api/admin/tenants/{id}/usage [get]
func (h *TenantUsageHandler) AdminUsage(w http.ResponseWriter, r *http.Request) {
	h.usage(w, r, chi.URLParam(r, "id"))
}

// billing reports what the Settings page may offer this tenant.
//
// It replaces the claim code, which existed only because the old provider had
// no way to carry a tenant id through checkout. A payment is bound to a tenant
// by the metadata on its Checkout session now, so there is nothing for a
// customer to copy and nothing for them to forget.
func (h *TenantUsageHandler) billing(r *http.Request, tenantID string) billingState {
	if stripeReady {
		st := billingState{Provider: "stripe"}
		if h.store != nil {
			if t, err := h.store.GetTenant(r.Context(), tenantID); err == nil && t != nil {
				st.Manageable = t.StripeCustomerID != ""
			}
		}
		return st
	}
	if upgradeURL != "" {
		return billingState{Provider: "kofi"}
	}
	return billingState{}
}

// stripeReady is set once at startup, beside SetUpgradeURL, so the usage
// endpoint can tell the UI which surface to draw without reaching for config.
var stripeReady bool

// SetStripeReady says whether checkout starts inside the Hub.
func SetStripeReady(v bool) { stripeReady = v }

func (h *TenantUsageHandler) usage(w http.ResponseWriter, r *http.Request, tenantID string) {
	if tenantID == "" {
		writeError(w, http.StatusBadRequest, "tenant required")
		return
	}
	u, err := h.quota.Usage(r.Context(), tenantID)
	if err != nil {
		// A tenant nobody has heard of is a 404, not a server error: the admin
		// variant takes an id from the URL and mistyping it should say so.
		// Both errors are checked because GetTenant surfaces the driver's
		// sql.ErrNoRows in both dialects rather than store.ErrNotFound.
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "tenant not found")
			return
		}
		slog.Error("tenant: usage lookup failed", "tenant", tenantID, "error", err)
		writeError(w, http.StatusInternalServerError, "could not read usage")
		return
	}
	resp := usageResponse{Usage: u, UpgradeURL: upgradeURL, Billing: h.billing(r, tenantID)}
	for _, name := range plans.Names() {
		resp.Tiers = append(resp.Tiers, tierResponse{
			Plan:    name,
			Devices: plans.For(name).Devices,
			Current: name == u.Plan,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}
