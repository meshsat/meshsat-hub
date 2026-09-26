package api

import (
	"log/slog"
	"net/http"

	"github.com/meshsat/meshsat-hub/internal/audit"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// logPlatformAction records what a platform operator did to a tenant, twice:
// on the tenant's own chain, where the customer can read it in their Audit
// log, and on the platform tenant's chain, where the operator's history
// lives. Every operator action on a customer goes through here
// (MESHSAT-1366). An action on the platform tenant itself is written once.
func logPlatformAction(r *http.Request, a *audit.Service, tenantID, action, detail string) {
	if a == nil {
		return
	}
	actor, ip := operatorEmail(r), clientIPFromRequest(r)
	if err := a.Log(r.Context(), tenantID, action, actor, detail, ip); err != nil {
		slog.Warn("audit: failed to log "+action, "tenant", tenantID, "error", err)
	}
	if tenantID == store.DefaultTenantID {
		return
	}
	mirrored := "tenant=" + tenantID
	if detail != "" {
		mirrored += " " + detail
	}
	if err := a.Log(r.Context(), store.DefaultTenantID, action, actor, mirrored, ip); err != nil {
		slog.Warn("audit: failed to mirror "+action+" to the platform chain", "tenant", tenantID, "error", err)
	}
}
