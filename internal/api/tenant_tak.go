package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/meshsat/meshsat-hub/internal/audit"
	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// Hosted TAK, from the customer's side (MESHSAT-1037): turn the tenant's own TAK
// server on, and see where it has got to.
//
// The Hub is the only party that talks to a TAK server (owner, 1 Oct 2026, epic
// MESHSAT-1459): kits and the MeshSat apps reach it through the Hub over MQTT,
// and the Hub forwards to the tenant's hosted server and reads it back. Phones
// running plain TAK apps no longer connect: the public port 8089 closed on
// 11 Nov 2026 (MESHSAT-1466), and with it went the accounts, the enrolment
// packages and the per-plan ceiling on accounts that this handler used to serve.
//
// # Why internal/takhosted is behind an interface here
//
// internal/routing imports this package, and routing is on an ingest path. An
// import of internal/takhosted would therefore put the Kubernetes custom-resource
// client and its in-cluster REST config behind every satellite message the Hub
// receives. That is the coupling that already forces internal/quota's invariant
// test to scan source text rather than the dependency graph (MESHSAT-992), and
// one instance of it is enough. So this package names the one thing it needs and
// cmd/meshsat-hub supplies it.

// TAKProvisioner creates the tenant's hosted OpenTAKServer. Implemented by
// takhosted.Provisioner.
type TAKProvisioner interface {
	// EnableTAK is idempotent and returns the instance label.
	EnableTAK(ctx context.Context, tenantID string) (string, error)
}

// TenantTAKHandler serves /api/tenant/tak.
type TenantTAKHandler struct {
	store store.Store
	audit *audit.Service
	prov  TAKProvisioner
	log   *slog.Logger
}

// NewTenantTAKHandler creates the handler. It is inert until SetTAK supplies the
// provisioner, which is what a Hub running without hosted TAK wants.
func NewTenantTAKHandler(s store.Store, a *audit.Service) *TenantTAKHandler {
	return &TenantTAKHandler{store: s, audit: a, log: slog.Default()}
}

// SetTAK attaches the hosted-TAK provisioner, in the shape of DeviceHandler.SetQuota.
func (h *TenantTAKHandler) SetTAK(p TAKProvisioner) {
	h.prov = p
}

type takStatusResponse struct {
	// Enabled is whether this tenant has a server at all.
	Enabled bool `json:"enabled"`
	// Phase is the operator's own word for where it has got to: Provisioning,
	// Ready, Failed and so on.
	//
	// There is deliberately no host and no port: the instance's address is an
	// in-cluster service name that only the Hub dials, and since MESHSAT-1466 no
	// phone connects to the Hub's TAK port either.
	Phase string `json:"phase,omitempty"`
}

// Status reports whether the tenant has a hosted TAK server and its phase.
//
// @Summary      Hosted TAK status
// @Tags         tenant
// @Produce      json
// @Success      200  {object}  takStatusResponse
// @Failure      500  {object}  map[string]string
// @Router       /api/tenant/tak [get]
func (h *TenantTAKHandler) Status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := hubauth.TenantIDFromContext(ctx)

	var out takStatusResponse
	inst, err := h.store.GetTAKInstance(ctx, tenantID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Not an error. Most tenants will never turn TAK on.
		out.Enabled = false
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not read the TAK status")
		return
	default:
		out.Enabled = true
		out.Phase = inst.Phase
	}
	writeJSON(w, http.StatusOK, out)
}

// Enable creates the tenant's own OpenTAKServer (owner-only, idempotent).
//
// @Summary      Turn hosted TAK on
// @Tags         tenant
// @Produce      json
// @Success      202  {object}  takStatusResponse  "being built"
// @Success      200  {object}  takStatusResponse  "already on"
// @Failure      503  {object}  map[string]string
// @Router       /api/tenant/tak [post]
func (h *TenantTAKHandler) Enable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := hubauth.TenantIDFromContext(ctx)

	if h.prov == nil {
		writeError(w, http.StatusServiceUnavailable, "hosted TAK is not available on this Hub")
		return
	}

	existing, err := h.store.GetTAKInstance(ctx, tenantID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "could not read the TAK status")
		return
	}
	already := existing != nil && err == nil

	if _, err := h.prov.EnableTAK(ctx, tenantID); err != nil {
		h.log.Error("tak: enabling hosted TAK failed", "tenant", tenantID, "error", err)
		writeError(w, http.StatusServiceUnavailable,
			"could not start building your TAK server; please try again shortly")
		return
	}

	if !already {
		h.logAudit(r, tenantID, "tak_enabled", "hosted TAK requested")
	}

	out := takStatusResponse{Enabled: true, Phase: "Provisioning"}
	if existing != nil {
		out.Phase = existing.Phase
	}
	if already {
		writeJSON(w, http.StatusOK, out)
		return
	}
	writeJSON(w, http.StatusAccepted, out)
}

// logAudit records a TAK action. A failure to write one never fails the
// customer's request.
func (h *TenantTAKHandler) logAudit(r *http.Request, tenantID, action, detail string) {
	if h.audit == nil {
		return
	}
	actor := operatorEmail(r)
	if err := h.audit.Log(r.Context(), tenantID, action, actor, detail, clientIPFromRequest(r)); err != nil {
		h.log.Warn("tak: audit write failed", "action", action, "tenant", tenantID, "error", err)
	}
}
