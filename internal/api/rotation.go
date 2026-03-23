package api

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/bridge"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
)

// RotationHandler provides REST endpoints for secret rotation.
type RotationHandler struct {
	store     store.Store
	commander *bridge.Commander
}

// NewRotationHandler creates a new rotation API handler.
func NewRotationHandler(s store.Store, cmdr *bridge.Commander) *RotationHandler {
	return &RotationHandler{store: s, commander: cmdr}
}

type rotateKeyResponse struct {
	Key       string     `json:"key"` // plaintext — shown once
	ID        string     `json:"id"`
	KeyPrefix string     `json:"key_prefix"`
	Role      string     `json:"role"`
	Label     string     `json:"label"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// RotateAPIKey force-rotates an API key, generating a new secret.
// @Summary Rotate API key
// @Description Generates a new secret for an existing API key. Returns the new plaintext key exactly once.
// @Tags auth
// @Produce json
// @Param id path string true "Key ID"
// @Success 200 {object} rotateKeyResponse
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/auth/keys/{id}/rotate [post]
func (h *RotationHandler) RotateAPIKey(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing key id")
		return
	}

	// Get existing key.
	existing, err := h.store.GetAPIKeyByID(r.Context(), tid, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "api key not found")
		return
	}

	// Generate new key material.
	plaintext, hash, prefix, err := auth.GenerateAPIKey()
	if err != nil {
		slog.Error("api key rotation: generation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "key generation failed")
		return
	}

	// Compute new expiry based on rotation_days if set.
	var newExpiry time.Time
	if existing.RotationDays > 0 {
		newExpiry = time.Now().Add(time.Duration(existing.RotationDays) * 24 * time.Hour)
	}

	if err := h.store.UpdateAPIKeySecret(r.Context(), tid, id, hash, prefix, newExpiry); err != nil {
		slog.Error("api key rotation: update failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update key")
		return
	}

	resp := rotateKeyResponse{
		Key:       plaintext,
		ID:        existing.ID,
		KeyPrefix: prefix,
		Role:      existing.Role,
		Label:     existing.Label,
	}
	if !newExpiry.IsZero() {
		resp.ExpiresAt = &newExpiry
	}

	slog.Info("api key rotated", "key_id", id, "tenant", tid)
	writeJSON(w, http.StatusOK, resp)
}

type rotateBridgeCredentialsResponse struct {
	BridgeID string `json:"bridge_id"`
	Username string `json:"username"`
	Password string `json:"password"`
	MQTTURL  string `json:"mqtt_url"`
}

// RotateBridgeCredentials force-rotates MQTT credentials for a bridge.
// @Summary Rotate bridge MQTT credentials
// @Description Generates a new MQTT password for the bridge. Returns the new plaintext password exactly once.
// @Tags bridges
// @Produce json
// @Param id path string true "Bridge ID"
// @Success 200 {object} rotateBridgeCredentialsResponse
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/bridges/{id}/credentials/rotate [post]
func (h *RotationHandler) RotateBridgeCredentials(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tid := auth.TenantIDFromContext(r.Context())

	if _, err := h.store.GetBridge(r.Context(), tid, id); err != nil {
		writeError(w, http.StatusNotFound, "bridge not found")
		return
	}

	// Generate 32-byte random password (64 hex chars).
	passwordBytes := make([]byte, 32)
	if _, err := rand.Read(passwordBytes); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate password")
		return
	}
	password := hex.EncodeToString(passwordBytes)

	// Hash with bcrypt (cost 10).
	hashBytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}

	username := id // MQTT username = bridge ID
	if err := h.store.SetBridgeCredentials(r.Context(), tid, id, username, string(hashBytes)); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to store credentials")
		return
	}

	// Publish new credentials to bridge via MQTT command if commander is available.
	if h.commander != nil {
		cmd := bridge.CredentialUpdateCommand(username, password)
		if _, err := h.commander.SendCommand(r.Context(), id, cmd); err != nil {
			slog.Warn("bridge credential rotation: failed to push credentials via MQTT",
				"bridge_id", id, "error", err)
			// Non-fatal — credentials are stored, bridge will get them on next connect.
		}
	}

	mqttURL := os.Getenv("MESHSAT_MQTT_PUBLIC_URL")
	if mqttURL == "" {
		mqttURL = "mqtt://hub.meshsat.net:6071"
	}

	slog.Info("bridge credentials rotated", "bridge_id", id, "tenant", tid)
	writeJSON(w, http.StatusOK, rotateBridgeCredentialsResponse{
		BridgeID: id,
		Username: username,
		Password: password,
		MQTTURL:  mqttURL,
	})
}
