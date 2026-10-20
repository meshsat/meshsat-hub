package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/meshsat/meshsat-hub/internal/audit"
	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/directory"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// DirectoryKeyRotateHandler rotates the Hub's directory signing key with a
// signed rotation record per tenant (contract C7.6), so every bridge that
// pinned the old key can follow. Platform admin only: the key is one for
// the whole Hub. [MESHSAT-1584 C7]
type DirectoryKeyRotateHandler struct {
	store  store.Store
	dir    *directory.SQLStore
	anchor *directory.TrustAnchor
	audit  *audit.Service
}

// NewDirectoryKeyRotateHandler builds the handler; a nil audit logs nothing.
func NewDirectoryKeyRotateHandler(s store.Store, dir *directory.SQLStore, anchor *directory.TrustAnchor, a *audit.Service) *DirectoryKeyRotateHandler {
	return &DirectoryKeyRotateHandler{store: s, dir: dir, anchor: anchor, audit: a}
}

// Rotate replaces the directory signing key.
// @Summary Rotate the directory signing key
// @Description Generates a new directory signing key and writes, for every tenant, a rotation record signed by the old key that names the new one; then makes the new key current. Bridges get the records they are missing before their next snapshot. A rotation that stopped part-way is finished by calling this again.
// @Tags directory
// @Produce json
// @Success 200 {object} directory.RotationResult
// @Failure 409 {object} map[string]string "A rotation from this key is already under way"
// @Failure 500 {object} map[string]string
// @Failure 503 {object} map[string]string "No directory signing key or directory store"
// @Router /api/admin/directory/signing-key/rotate [post]
func (h *DirectoryKeyRotateHandler) Rotate(w http.ResponseWriter, r *http.Request) {
	if h.anchor == nil || h.dir == nil {
		writeError(w, http.StatusServiceUnavailable, "directory signing is not configured")
		return
	}
	tenants, err := h.store.ListTenants(r.Context())
	if err != nil {
		writeInternalError(w, err, "")
		return
	}
	ids := make([]string, 0, len(tenants)+1)
	seenDefault := false
	for _, t := range tenants {
		ids = append(ids, t.ID)
		seenDefault = seenDefault || t.ID == store.DefaultTenantID
	}
	if !seenDefault {
		ids = append(ids, store.DefaultTenantID)
	}
	// The rotation must not stop because the caller went away: it resumes
	// either way, but finishing now is better.
	res, err := directory.RotateSigningKey(context.WithoutCancel(r.Context()), h.store, h.dir, h.anchor, ids)
	if errors.Is(err, directory.ErrRotationInProgress) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeInternalError(w, err, "")
		return
	}
	auditRequest(h.audit, r, auth.TenantIDFromContext(r.Context()), "directory_signing_key_rotated",
		fmt.Sprintf("old=%s new=%s tenants=%d records=%d resumed=%v", res.OldKeyID, res.NewKeyID, res.Tenants, res.Records, res.Resumed))
	writeJSON(w, http.StatusOK, res)
}
