package api

import (
	"fmt"
	"github.com/meshsat/meshsat-hub/internal/audit"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// APIKeyHandler handles API key management endpoints.
type APIKeyHandler struct {
	store store.Store
	audit *audit.Service
}

// NewAPIKeyHandler returns a new API key handler.
func NewAPIKeyHandler(s store.Store) *APIKeyHandler {
	return &APIKeyHandler{store: s}
}

// SetAudit makes key creation and deletion audit events. A credential that
// can be minted and revoked without a trace is a credential whose history
// nobody can reconstruct (ASVS V16; posture item 6).
func (h *APIKeyHandler) SetAudit(a *audit.Service) { h.audit = a }

func (h *APIKeyHandler) auditLog(r *http.Request, action, detail string) {
	if h.audit == nil {
		return
	}
	actor := "unknown"
	if u := auth.FromContext(r.Context()); u != nil {
		if u.Email != "" {
			actor = u.Email
		} else if u.ID != "" {
			actor = u.ID
		}
	}
	if err := h.audit.Log(r.Context(), auth.TenantIDFromContext(r.Context()), action, actor, detail, clientIPFromRequest(r)); err != nil {
		slog.Warn("audit: failed to log "+action, "error", err)
	}
}

type createKeyRequest struct {
	Label      string `json:"label"`
	Role       string `json:"role"`
	DeviceIMEI string `json:"device_imei,omitempty"`
	ExpiresIn  string `json:"expires_in,omitempty"` // Go duration string, e.g. "720h"
	// PlatformAdmin requests the cross-tenant axis (MESHSAT-1209). Refused unless
	// the CALLER already holds it — see CreateKey. This route is open to any
	// tenant owner, so an ungated field here would be a cross-tenant escalation.
	PlatformAdmin bool `json:"platform_admin,omitempty"`
}

type createKeyResponse struct {
	Key       string     `json:"key"` // plaintext — shown once
	ID        string     `json:"id"`
	KeyPrefix string     `json:"key_prefix"`
	Role      string     `json:"role"`
	Label     string     `json:"label"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// CreateKey generates a new API key. Owner-only.
// @Summary Create API key
// @Tags auth
// @Accept json
// @Produce json
// @Param body body createKeyRequest true "Key parameters"
// @Success 201 {object} createKeyResponse
// @Failure 400 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Router /api/auth/keys [post]
func (h *APIKeyHandler) CreateKey(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())

	var req createKeyRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Validate role.
	switch req.Role {
	case auth.RoleViewer, auth.RoleOperator, auth.RoleOwner:
		// ok
	case "":
		req.Role = auth.RoleViewer
	default:
		writeError(w, http.StatusBadRequest, "invalid role: must be viewer, operator, or owner")
		return
	}

	// ⚠ THE GATE (MESHSAT-1209). POST /api/auth/keys is gated at RequireRole(owner),
	// so ANY tenant owner reaches this handler. Without the check below, a customer's
	// owner could mint themselves a key carrying PlatformAdmin and act across every
	// tenant — the same shape as the ungated provision route that handed a viewer a
	// bridge private key. Only a caller who ALREADY holds the axis may pass it on.
	//
	// 403 rather than silently dropping the field: a caller who asked for platform
	// admin and got an ordinary key would believe they hold authority they do not,
	// and would find out at the first cross-tenant call.
	if req.PlatformAdmin {
		if u := auth.FromContext(r.Context()); u == nil || !u.PlatformAdmin {
			writeError(w, http.StatusForbidden, "platform_admin keys may only be created by a platform administrator")
			return
		}
	}

	plaintext, hash, prefix, err := auth.GenerateAPIKey()
	if err != nil {
		slog.Error("api key generation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "key generation failed")
		return
	}

	key := &store.APIKey{
		KeyHash:       hash,
		KeyPrefix:     prefix,
		Role:          req.Role,
		Label:         req.Label,
		DeviceIMEI:    req.DeviceIMEI,
		PlatformAdmin: req.PlatformAdmin,
	}

	if req.ExpiresIn != "" {
		dur, err := time.ParseDuration(req.ExpiresIn)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid expires_in duration")
			return
		}
		exp := time.Now().Add(dur)
		key.ExpiresAt = exp
	}

	if err := h.store.CreateAPIKey(r.Context(), tid, key); err != nil {
		slog.Error("api key creation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "key creation failed")
		return
	}

	resp := createKeyResponse{
		Key:       plaintext,
		ID:        key.ID,
		KeyPrefix: prefix,
		Role:      req.Role,
		Label:     req.Label,
	}
	if !key.ExpiresAt.IsZero() {
		resp.ExpiresAt = &key.ExpiresAt
	}
	// The prefix identifies the key without being the key.
	h.auditLog(r, "api_key_created", fmt.Sprintf("id=%s prefix=%s role=%s platform_admin=%t label=%q", key.ID, prefix, req.Role, key.PlatformAdmin, req.Label))

	writeJSON(w, http.StatusCreated, resp)
}

// ListKeys returns all API keys for the tenant (without hashes).
// @Summary List API keys
// @Tags auth
// @Produce json
// @Success 200 {array} store.APIKey
// @Router /api/auth/keys [get]
func (h *APIKeyHandler) ListKeys(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())

	keys, err := h.store.ListAPIKeys(r.Context(), tid)
	if err != nil {
		slog.Error("list api keys failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list keys")
		return
	}
	if keys == nil {
		keys = []store.APIKey{}
	}
	writeJSON(w, http.StatusOK, keys)
}

// DeleteKey revokes an API key by ID.
// @Summary Revoke API key
// @Tags auth
// @Param id path string true "Key ID"
// @Success 204
// @Failure 403 {object} map[string]string
// @Router /api/auth/keys/{id} [delete]
func (h *APIKeyHandler) DeleteKey(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing key id")
		return
	}

	if err := h.store.DeleteAPIKey(r.Context(), tid, id); err != nil {
		slog.Error("delete api key failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete key")
		return
	}
	h.auditLog(r, "api_key_deleted", "id="+id)
	w.WriteHeader(http.StatusNoContent)
}
