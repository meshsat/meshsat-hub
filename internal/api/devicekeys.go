package api

import (
	"encoding/hex"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/bridge"
	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// DeviceKeyHandler handles device encryption key management endpoints.
type DeviceKeyHandler struct {
	store     store.Store
	keyStore  *hubcrypto.KeyStore
	commander *bridge.Commander
}

// NewDeviceKeyHandler returns a new device key handler.
func NewDeviceKeyHandler(s store.Store, ks *hubcrypto.KeyStore, cmdr *bridge.Commander) *DeviceKeyHandler {
	return &DeviceKeyHandler{store: s, keyStore: ks, commander: cmdr}
}

type createDeviceKeyRequest struct {
	Mode string `json:"mode"` // "decrypt" or "passthrough"
}

type importDeviceKeyRequest struct {
	KeyHex string `json:"key_hex"` // hex-encoded AES-256 key (64 hex chars = 32 bytes)
	Mode   string `json:"mode"`    // "decrypt" or "passthrough"
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

// ImportKey imports an existing encryption key for a device.
//
//	@Summary      Import device encryption key
//	@Description  Imports an existing AES-256 key (hex-encoded) for a device or global key ID (e.g. "sms").
//	@Tags         devices
//	@Accept       json
//	@Produce      json
//	@Param        imei  path  string  true  "Device IMEI or key ID (e.g. sms)"
//	@Param        body  body  importDeviceKeyRequest  true  "Key to import"
//	@Success      201  {object}  createDeviceKeyResponse
//	@Failure      400  {object}  map[string]string
//	@Router       /api/devices/{imei}/keys/import [post]
func (h *DeviceKeyHandler) ImportKey(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	imei := chi.URLParam(r, "imei")
	if imei == "" {
		writeError(w, http.StatusBadRequest, "missing imei")
		return
	}

	var req importDeviceKeyRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.KeyHex == "" {
		writeError(w, http.StatusBadRequest, "key_hex is required")
		return
	}

	keyBytes, err := hex.DecodeString(req.KeyHex)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid hex key")
		return
	}
	if len(keyBytes) != 32 {
		writeError(w, http.StatusBadRequest, "key must be 32 bytes (64 hex chars)")
		return
	}

	switch req.Mode {
	case "decrypt", "passthrough":
	case "":
		req.Mode = "decrypt"
	default:
		writeError(w, http.StatusBadRequest, "invalid mode: must be decrypt or passthrough")
		return
	}

	entry, err := h.keyStore.StoreKey(imei, keyBytes, req.Mode)
	if err != nil {
		slog.Error("device key import failed", "error", err)
		writeError(w, http.StatusInternalServerError, "key import failed")
		return
	}

	dk := &store.DeviceKey{
		DeviceIMEI: imei,
		KeyHash:    entry.KeyHashHex,
		Mode:       req.Mode,
	}
	if req.Mode == "decrypt" {
		dk.KeyHex = req.KeyHex
	}

	if err := h.store.CreateDeviceKey(r.Context(), tid, dk); err != nil {
		slog.Error("device key persistence failed", "error", err)
		writeError(w, http.StatusInternalServerError, "key persistence failed")
		return
	}

	slog.Info("crypto: key imported", "device", imei, "mode", req.Mode)

	resp := createDeviceKeyResponse{
		ID:      dk.ID,
		KeyHash: entry.KeyHashHex,
		Mode:    req.Mode,
	}
	writeJSON(w, http.StatusCreated, resp)
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

type rotateDistributeRequest struct {
	ChannelType string   `json:"channel_type"` // e.g. "sms", "mesh", "iridium"
	Address     string   `json:"address"`      // e.g. "+31653618463", "!abcd1234"
	BridgeIDs   []string `json:"bridge_ids"`   // target bridges to push key to
}

type distributeKeyRequest struct {
	BridgeIDs []string `json:"bridge_ids"` // target bridges
}

type rotateDistributeResponse struct {
	KeyHash     string                  `json:"key_hash"`
	Version     int                     `json:"version"`
	Distributed []distributeResultEntry `json:"distributed"`
}

type distributeResultEntry struct {
	BridgeID string `json:"bridge_id"`
	Status   string `json:"status"` // "ok" or "error"
	Error    string `json:"error,omitempty"`
}

// RotateAndDistribute generates a new encryption key and pushes it to the specified bridges.
//
//	@Summary      Rotate device key and distribute to bridges
//	@Description  Generates a new AES-256-GCM key for a device, persists it, and pushes a key_rotate command to the specified bridges via MQTT.
//	@Tags         devices
//	@Accept       json
//	@Produce      json
//	@Param        imei  path  string  true  "Device IMEI"
//	@Param        body  body  rotateDistributeRequest  true  "Rotation parameters"
//	@Success      200  {object}  rotateDistributeResponse
//	@Failure      400  {object}  map[string]string
//	@Failure      500  {object}  map[string]string
//	@Router       /api/devices/{imei}/keys/rotate [post]
func (h *DeviceKeyHandler) RotateAndDistribute(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	imei := chi.URLParam(r, "imei")
	if imei == "" {
		writeError(w, http.StatusBadRequest, "missing imei")
		return
	}

	var req rotateDistributeRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.ChannelType == "" {
		writeError(w, http.StatusBadRequest, "channel_type is required")
		return
	}
	if req.Address == "" {
		writeError(w, http.StatusBadRequest, "address is required")
		return
	}

	// Generate a new key via the in-memory keystore.
	entry, rawKey, err := h.keyStore.GenerateAndStore(imei, "decrypt")
	if err != nil {
		slog.Error("key rotation generation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "key generation failed")
		return
	}

	keyHex := hex.EncodeToString(rawKey)

	dk := &store.DeviceKey{
		DeviceIMEI: imei,
		KeyHash:    entry.KeyHashHex,
		KeyHex:     keyHex,
		Mode:       "decrypt",
	}
	if err := h.store.CreateDeviceKey(r.Context(), tid, dk); err != nil {
		slog.Error("key rotation persistence failed", "error", err)
		writeError(w, http.StatusInternalServerError, "key persistence failed")
		return
	}

	// Distribute key to each target bridge via MQTT command.
	results := make([]distributeResultEntry, 0, len(req.BridgeIDs))
	for _, bid := range req.BridgeIDs {
		cmd := bridge.KeyRotateCommand(req.ChannelType, req.Address, keyHex, entry.Version)
		_, err := h.commander.SendCommand(r.Context(), bid, cmd)
		res := distributeResultEntry{BridgeID: bid, Status: "ok"}
		if err != nil {
			res.Status = "error"
			res.Error = err.Error()
			slog.Warn("key rotation distribute failed", "bridge", bid, "error", err)
		}
		results = append(results, res)
	}

	slog.Info("crypto: key rotated and distributed", "device", imei, "version", entry.Version, "bridges", len(req.BridgeIDs))

	writeJSON(w, http.StatusOK, rotateDistributeResponse{
		KeyHash:     entry.KeyHashHex,
		Version:     entry.Version,
		Distributed: results,
	})
}

// DistributeKey pushes the latest encryption key to the specified bridges.
//
//	@Summary      Distribute existing device key to bridges
//	@Description  Retrieves the latest key for a device and pushes a key_rotate command to the specified bridges via MQTT.
//	@Tags         devices
//	@Accept       json
//	@Produce      json
//	@Param        imei  path  string  true  "Device IMEI"
//	@Param        body  body  distributeKeyRequest  true  "Distribution parameters"
//	@Success      200  {object}  rotateDistributeResponse
//	@Failure      400  {object}  map[string]string
//	@Failure      404  {object}  map[string]string
//	@Failure      500  {object}  map[string]string
//	@Router       /api/devices/{imei}/keys/distribute [post]
func (h *DeviceKeyHandler) DistributeKey(w http.ResponseWriter, r *http.Request) {
	imei := chi.URLParam(r, "imei")
	if imei == "" {
		writeError(w, http.StatusBadRequest, "missing imei")
		return
	}

	var req distributeKeyRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if len(req.BridgeIDs) == 0 {
		writeError(w, http.StatusBadRequest, "bridge_ids is required")
		return
	}

	// Get the latest key for this device.
	entry, err := h.keyStore.GetLatest(imei)
	if err != nil {
		writeError(w, http.StatusNotFound, "no key found for device")
		return
	}

	if entry.KeyHex == "" {
		writeError(w, http.StatusBadRequest, "key material not available (passthrough mode)")
		return
	}

	// Distribute key to each target bridge via MQTT command.
	// Use empty channel_type and address — bridge uses key for all channels on this device.
	results := make([]distributeResultEntry, 0, len(req.BridgeIDs))
	for _, bid := range req.BridgeIDs {
		cmd := bridge.KeyRotateCommand("", "", entry.KeyHex, entry.Version)
		_, err := h.commander.SendCommand(r.Context(), bid, cmd)
		res := distributeResultEntry{BridgeID: bid, Status: "ok"}
		if err != nil {
			res.Status = "error"
			res.Error = err.Error()
			slog.Warn("key distribute failed", "bridge", bid, "error", err)
		}
		results = append(results, res)
	}

	slog.Info("crypto: key distributed", "device", imei, "version", entry.Version, "bridges", len(req.BridgeIDs))

	writeJSON(w, http.StatusOK, rotateDistributeResponse{
		KeyHash:     entry.KeyHashHex,
		Version:     entry.Version,
		Distributed: results,
	})
}
