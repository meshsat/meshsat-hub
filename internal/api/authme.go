package api

import (
	"net/http"

	"github.com/meshsat/meshsat-hub/internal/auth"
)

// AuthMeHandler returns the current authenticated user info.
// @Summary Get current user
// @Tags auth
// @Produce json
// @Success 200 {object} auth.User
// @Router /api/auth/me [get]
func AuthMeHandler(w http.ResponseWriter, r *http.Request) {
	user := auth.FromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	tid := auth.TenantIDFromContext(r.Context())
	resp := map[string]interface{}{
		"id":        user.ID,
		"email":     user.Email,
		"name":      user.Name,
		"roles":     user.Roles,
		"tenant_id": tid,
		// True for members of the platform-admin IdP group (may act across tenants).
		"platform_admin": user.PlatformAdmin,
	}
	writeJSON(w, http.StatusOK, resp)
}
