package api

import (
	"log/slog"
	"net/http"

	"github.com/meshsat/meshsat-hub/internal/audit"
	"github.com/meshsat/meshsat-hub/internal/auth"
)

// auditRequest writes one audit entry for r in tenantID. The actor is the
// caller: e-mail, else user id, else "unknown". The detail names what was
// changed and never carries a secret (a password, key or nonce).
//
// The bridge routes had no audit at all (MESHSAT-1308): whether a bridge had
// ever been created over another tenant's (MESHSAT-1307) could not be told
// from the log, and the routes that mint a bridge's password and private key
// left no trace.
func auditRequest(a *audit.Service, r *http.Request, tenantID, action, detail string) {
	if a == nil {
		return
	}
	if err := a.Log(r.Context(), tenantID, action, requestActor(r), detail, clientIPFromRequest(r)); err != nil {
		slog.Warn("audit: failed to log "+action, "error", err)
	}
}

func requestActor(r *http.Request) string {
	if u := auth.FromContext(r.Context()); u != nil {
		if u.Email != "" {
			return u.Email
		}
		if u.ID != "" {
			return u.ID
		}
	}
	return "unknown"
}
