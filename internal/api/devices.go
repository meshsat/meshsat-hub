package api

import (
	"encoding/json"
	"net/http"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/go-chi/chi/v5"
)

// DeviceHandler provides REST endpoints for device management.
type DeviceHandler struct {
	store store.Store
}

// NewDeviceHandler creates a new device API handler.
func NewDeviceHandler(s store.Store) *DeviceHandler {
	return &DeviceHandler{store: s}
}

// ListDevices returns all registered devices.
// GET /api/devices
func (h *DeviceHandler) ListDevices(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	devices, err := h.store.ListDevices(r.Context(), tid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if devices == nil {
		devices = []store.Device{}
	}
	writeJSON(w, http.StatusOK, devices)
}

// GetDevice returns a single device by IMEI.
// GET /api/devices/{imei}
func (h *DeviceHandler) GetDevice(w http.ResponseWriter, r *http.Request) {
	imei := chi.URLParam(r, "imei")
	tid := auth.TenantIDFromContext(r.Context())
	device, err := h.store.GetDevice(r.Context(), tid, imei)
	if err != nil {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	writeJSON(w, http.StatusOK, device)
}

// CreateDevice registers a new device.
// POST /api/devices
func (h *DeviceHandler) CreateDevice(w http.ResponseWriter, r *http.Request) {
	var dev store.Device
	if err := json.NewDecoder(r.Body).Decode(&dev); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if dev.IMEI == "" {
		writeError(w, http.StatusBadRequest, "imei is required")
		return
	}
	if dev.Type == "" {
		dev.Type = "rockblock"
	}
	tid := auth.TenantIDFromContext(r.Context())
	if err := h.store.CreateDevice(r.Context(), tid, &dev); err != nil {
		writeError(w, http.StatusConflict, "device already exists or error: "+err.Error())
		return
	}
	created, _ := h.store.GetDevice(r.Context(), tid, dev.IMEI)
	writeJSON(w, http.StatusCreated, created)
}

// UpdateDevice updates a device's label, type, notes.
// PUT /api/devices/{imei}
func (h *DeviceHandler) UpdateDevice(w http.ResponseWriter, r *http.Request) {
	imei := chi.URLParam(r, "imei")
	tid := auth.TenantIDFromContext(r.Context())
	existing, err := h.store.GetDevice(r.Context(), tid, imei)
	if err != nil {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}

	var req struct {
		Label string `json:"label"`
		Type  string `json:"type"`
		Notes string `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	existing.Label = req.Label
	existing.Type = req.Type
	existing.Notes = req.Notes
	if err := h.store.UpdateDevice(r.Context(), tid, existing); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	updated, _ := h.store.GetDevice(r.Context(), tid, imei)
	writeJSON(w, http.StatusOK, updated)
}

// DeleteDevice removes a device.
// DELETE /api/devices/{imei}
func (h *DeviceHandler) DeleteDevice(w http.ResponseWriter, r *http.Request) {
	imei := chi.URLParam(r, "imei")
	tid := auth.TenantIDFromContext(r.Context())
	if _, err := h.store.GetDevice(r.Context(), tid, imei); err != nil {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	if err := h.store.DeleteDevice(r.Context(), tid, imei); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
