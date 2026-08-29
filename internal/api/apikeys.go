package api

import (
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
}

// NewAPIKeyHandler returns a new API key handler.
func NewAPIKeyHandler(s store.Store) *APIKeyHandler {
	return &APIKeyHandler{store: s}
}

type createKeyRequest struct {
	Label      string `json:"label"`
	Role       string `json:"role"`
	DeviceIMEI string `json:"device_imei,omitempty"`
	ExpiresIn  string `json:"expires_in,omitempty"` // Go duration string, e.g. "720h"
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

	plaintext, hash, prefix, err := auth.GenerateAPIKey()
	if err != nil {
		slog.Error("api key generation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "key generation failed")
		return
	}

	key := &store.APIKey{
		KeyHash:    hash,
		KeyPrefix:  prefix,
		Role:       req.Role,
		Label:      req.Label,
		DeviceIMEI: req.DeviceIMEI,
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
	w.WriteHeader(http.StatusNoContent)
}
