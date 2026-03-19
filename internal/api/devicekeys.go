package api

import (
	"encoding/hex"
	"log/slog"
	"net/http"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	hubcrypto "github.com/cubeos-app/meshsat-hub/internal/crypto"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/go-chi/chi/v5"
)

// DeviceKeyHandler handles device encryption key management endpoints.
type DeviceKeyHandler struct {
	store    store.Store
	keyStore *hubcrypto.KeyStore
}

// NewDeviceKeyHandler returns a new device key handler.
func NewDeviceKeyHandler(s store.Store, ks *hubcrypto.KeyStore) *DeviceKeyHandler {
	return &DeviceKeyHandler{store: s, keyStore: ks}
}

type createDeviceKeyRequest struct {
	Mode string `json:"mode"` // "decrypt" or "passthrough"
}

type createDeviceKeyResponse struct {
	ID      string `json:"id"`
	KeyHex  string `json:"key_hex,omitempty"` // plaintext — shown once
	KeyHash string `json:"key_hash"`
	Mode    string `json:"mode"`
}

// CreateKey generates a new encryption key for a device. Key plaintext is shown once.
//
//	@Summary      Create device encryption key
//	@Description  Generates a new AES-256-GCM key for a device. The key hex is returned once and stored as a hash.
//	@Tags         devices
//	@Accept       json
//	@Produce      json
//	@Param        imei  path  string  true  "Device IMEI"
//	@Param        body  body  createDeviceKeyRequest  true  "Key parameters"
//	@Success      201  {object}  createDeviceKeyResponse
//	@Failure      400  {object}  map[string]string
//	@Router       /api/devices/{imei}/keys [post]
func (h *DeviceKeyHandler) CreateKey(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	imei := chi.URLParam(r, "imei")
	if imei == "" {
		writeError(w, http.StatusBadRequest, "missing imei")
		return
	}

	var req createDeviceKeyRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	switch req.Mode {
	case "decrypt", "passthrough":
		// ok
	case "":
		req.Mode = "decrypt"
	default:
		writeError(w, http.StatusBadRequest, "invalid mode: must be decrypt or passthrough")
		return
	}

	// Generate key via the in-memory keystore (also registers for decryption).
	entry, rawKey, err := h.keyStore.GenerateAndStore(imei, req.Mode)
	if err != nil {
		slog.Error("device key generation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "key generation failed")
		return
	}

	keyHex := hex.EncodeToString(rawKey)

	dk := &store.DeviceKey{
		DeviceIMEI: imei,
		KeyHash:    entry.KeyHashHex,
		Mode:       req.Mode,
	}

	// In decrypt mode, store the key material so it persists across restarts.
	if req.Mode == "decrypt" {
		dk.KeyHex = keyHex
	}

	if err := h.store.CreateDeviceKey(r.Context(), tid, dk); err != nil {
		slog.Error("device key creation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "key creation failed")
		return
	}

	resp := createDeviceKeyResponse{
		ID:      dk.ID,
		KeyHex:  keyHex, // shown once regardless of mode
		KeyHash: entry.KeyHashHex,
		Mode:    req.Mode,
	}
	writeJSON(w, http.StatusCreated, resp)
}

// ListKeys returns all encryption keys for a device (without key material).
//
//	@Summary      List device encryption keys
//	@Tags         devices
//	@Produce      json
//	@Param        imei  path  string  true  "Device IMEI"
//	@Success      200  {array}  store.DeviceKey
//	@Router       /api/devices/{imei}/keys [get]
func (h *DeviceKeyHandler) ListKeys(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	imei := chi.URLParam(r, "imei")
	if imei == "" {
		writeError(w, http.StatusBadRequest, "missing imei")
		return
	}

	keys, err := h.store.ListDeviceKeys(r.Context(), tid, imei)
	if err != nil {
		slog.Error("list device keys failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list keys")
		return
	}
	if keys == nil {
		keys = []store.DeviceKey{}
	}
	writeJSON(w, http.StatusOK, keys)
}

// DeleteKey revokes a device encryption key by ID.
//
//	@Summary      Delete device encryption key
//	@Tags         devices
//	@Param        imei  path  string  true  "Device IMEI"
//	@Param        id    path  string  true  "Key ID"
//	@Success      204
//	@Router       /api/devices/{imei}/keys/{id} [delete]
func (h *DeviceKeyHandler) DeleteKey(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing key id")
		return
	}

	if err := h.store.DeleteDeviceKey(r.Context(), tid, id); err != nil {
		slog.Error("delete device key failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete key")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
