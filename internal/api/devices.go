package api

import (
	"log/slog"
	"net/http"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/cubeos-app/meshsat-hub/internal/wireguard"
	"github.com/go-chi/chi/v5"
)

// DeviceHandler provides REST endpoints for device management.
type DeviceHandler struct {
	store       store.Store
	provisioner *wireguard.Provisioner // nil if WireGuard disabled
}

// NewDeviceHandler creates a new device API handler.
func NewDeviceHandler(s store.Store) *DeviceHandler {
	return &DeviceHandler{store: s}
}

// SetProvisioner enables WireGuard auto-provisioning on device create/delete.
func (h *DeviceHandler) SetProvisioner(p *wireguard.Provisioner) {
	h.provisioner = p
}

// ListDevices returns all registered devices.
// @Summary List devices
// @Tags devices
// @Produce json
// @Success 200 {array} store.Device
// @Failure 500 {object} map[string]string
// @Router /api/devices [get]
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
// @Summary Get device by IMEI
// @Tags devices
// @Produce json
// @Param imei path string true "Device IMEI"
// @Success 200 {object} store.Device
// @Failure 404 {object} map[string]string
// @Router /api/devices/{imei} [get]
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

// deviceCreateResponse extends Device with optional WireGuard info.
type deviceCreateResponse struct {
	*store.Device
	Wireguard *store.DeviceWireguard `json:"wireguard,omitempty"`
}

// CreateDevice registers a new device.
// @Summary Register a new device
// @Description If WireGuard is enabled, a VPN peer is auto-provisioned and its config returned.
// @Tags devices
// @Accept json
// @Produce json
// @Param body body store.Device true "Device data (imei required)"
// @Success 201 {object} deviceCreateResponse
// @Failure 400 {object} map[string]string
// @Failure 409 {object} map[string]string
// @Router /api/devices [post]
func (h *DeviceHandler) CreateDevice(w http.ResponseWriter, r *http.Request) {
	var dev store.Device
	if err := readJSON(w, r, &dev); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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

	resp := deviceCreateResponse{Device: created}

	// Auto-provision WireGuard peer if enabled
	if h.provisioner != nil {
		vpnAddr, peer, err := h.provisioner.OnDeviceCreated(r.Context(), dev.IMEI)
		if err != nil {
			slog.Warn("wireguard: auto-provision failed (device created without VPN)", "imei", dev.IMEI, "error", err)
		} else {
			dw := &store.DeviceWireguard{
				DeviceIMEI: dev.IMEI,
				PeerID:     peer.ID,
				VPNAddress: vpnAddr,
				PublicKey:  peer.PublicKey,
			}
			if err := h.store.SaveDeviceWireguard(r.Context(), tid, dw); err != nil {
				slog.Error("wireguard: failed to save peer record", "imei", dev.IMEI, "error", err)
			}
			resp.Wireguard = dw
		}
	}

	writeJSON(w, http.StatusCreated, resp)
}

// UpdateDevice updates a device's label, type, notes.
// @Summary Update device
// @Tags devices
// @Accept json
// @Produce json
// @Param imei path string true "Device IMEI"
// @Param body body object true "Fields to update (label, type, notes)"
// @Success 200 {object} store.Device
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/devices/{imei} [put]
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
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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
// @Summary Delete device
// @Description Also removes the WireGuard peer if one was provisioned.
// @Tags devices
// @Param imei path string true "Device IMEI"
// @Success 204
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/devices/{imei} [delete]
func (h *DeviceHandler) DeleteDevice(w http.ResponseWriter, r *http.Request) {
	imei := chi.URLParam(r, "imei")
	tid := auth.TenantIDFromContext(r.Context())
	if _, err := h.store.GetDevice(r.Context(), tid, imei); err != nil {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}

	// Remove WireGuard peer if provisioned
	if h.provisioner != nil {
		h.provisioner.OnDeviceDeleted(r.Context(), imei)
		if err := h.store.DeleteDeviceWireguard(r.Context(), tid, imei); err != nil {
			slog.Debug("wireguard: no peer record to clean up", "imei", imei)
		}
	}

	if err := h.store.DeleteDevice(r.Context(), tid, imei); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetDeviceWireguard returns the WireGuard config for a device.
// @Summary Get device WireGuard config
// @Tags devices
// @Produce text/plain
// @Param imei path string true "Device IMEI"
// @Success 200 {string} string "WireGuard INI config"
// @Failure 404 {object} map[string]string
// @Failure 502 {object} map[string]string
// @Router /api/devices/{imei}/wireguard [get]
func (h *DeviceHandler) GetDeviceWireguard(w http.ResponseWriter, r *http.Request) {
	imei := chi.URLParam(r, "imei")
	tid := auth.TenantIDFromContext(r.Context())

	// Verify device exists
	if _, err := h.store.GetDevice(r.Context(), tid, imei); err != nil {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}

	// Check if WireGuard peer is provisioned
	if h.provisioner == nil {
		writeError(w, http.StatusNotFound, "wireguard not enabled")
		return
	}

	config, err := h.provisioner.GetDeviceConfig(r.Context(), imei)
	if err != nil {
		writeError(w, http.StatusNotFound, "no wireguard peer for this device")
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(config))
}
