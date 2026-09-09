package api

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/kofi"
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
	// UpgradeURL is where a person goes to pay. Empty when unconfigured, and
	// the UI hides the link rather than inventing one.
	UpgradeURL string `json:"upgrade_url,omitempty"`
	// ClaimCode goes in the Ko-fi message so the payment reaches this tenant
	// and not another. Generated on first read, because every tenant that
	// existed before tiers did needs one and nobody should have to ask.
	ClaimCode string `json:"claim_code,omitempty"`
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
//	@Failure      500  {object}  map[string]string
//	@Router       /api/admin/tenants/{id}/usage [get]
func (h *TenantUsageHandler) AdminUsage(w http.ResponseWriter, r *http.Request) {
	h.usage(w, r, chi.URLParam(r, "id"))
}

// claimCode returns the tenant's Ko-fi claim code, minting one the first time
// it is asked for. A failure here is not worth failing the whole response
// over: the page still shows the usage, just without the code.
func (h *TenantUsageHandler) claimCode(r *http.Request, tenantID string) string {
	if h.store == nil {
		return ""
	}
	t, err := h.store.GetTenant(r.Context(), tenantID)
	if err != nil || t == nil {
		return ""
	}
	if t.KofiClaimCode != "" {
		return t.KofiClaimCode
	}
	code, err := kofi.NewClaimCode()
	if err != nil {
		slog.Warn("tenant: could not mint a Ko-fi claim code", "tenant", tenantID, "error", err)
		return ""
	}
	t.KofiClaimCode = code
	if err := h.store.UpdateTenant(r.Context(), t); err != nil {
		slog.Warn("tenant: could not save a Ko-fi claim code", "tenant", tenantID, "error", err)
		return ""
	}
	return code
}

func (h *TenantUsageHandler) usage(w http.ResponseWriter, r *http.Request, tenantID string) {
	if tenantID == "" {
		writeError(w, http.StatusBadRequest, "tenant required")
		return
	}
	u, err := h.quota.Usage(r.Context(), tenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read usage")
		return
	}
	resp := usageResponse{Usage: u, UpgradeURL: upgradeURL, ClaimCode: h.claimCode(r, tenantID)}
	for _, name := range plans.Names() {
		resp.Tiers = append(resp.Tiers, tierResponse{
			Plan:    name,
			Devices: plans.For(name).Devices,
			Current: name == u.Plan,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}
