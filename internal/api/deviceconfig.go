package api

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/go-chi/chi/v5"
)

// DeviceConfigHandler handles device config versioning endpoints.
type DeviceConfigHandler struct {
	store store.Store
}

// NewDeviceConfigHandler returns a new handler.
func NewDeviceConfigHandler(s store.Store) *DeviceConfigHandler {
	return &DeviceConfigHandler{store: s}
}

type createConfigRequest struct {
	Config  json.RawMessage `json:"config"`
	Comment string          `json:"comment,omitempty"`
}

// GetLatest returns the latest config version for a device.
// @Summary Get latest device config
// @Tags device-config
// @Produce json
// @Param imei path string true "Device IMEI"
// @Success 200 {object} store.DeviceConfig
// @Failure 404 {object} map[string]string
// @Router /api/devices/{imei}/config [get]
func (h *DeviceConfigHandler) GetLatest(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	imei := chi.URLParam(r, "imei")

	cfg, err := h.store.GetDeviceConfigLatest(r.Context(), tid, imei)
	if err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "no config for device")
			return
		}
		slog.Error("get device config", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to get config")
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// GetVersion returns a specific config version.
// @Summary Get specific config version
// @Tags device-config
// @Produce json
// @Param imei path string true "Device IMEI"
// @Param version path int true "Version number"
// @Success 200 {object} store.DeviceConfig
// @Failure 404 {object} map[string]string
// @Router /api/devices/{imei}/config/{version} [get]
func (h *DeviceConfigHandler) GetVersion(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	imei := chi.URLParam(r, "imei")
	versionStr := chi.URLParam(r, "version")

	version, err := strconv.Atoi(versionStr)
	if err != nil || version < 1 {
		writeError(w, http.StatusBadRequest, "invalid version number")
		return
	}

	cfg, err := h.store.GetDeviceConfigVersion(r.Context(), tid, imei, version)
	if err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "version not found")
			return
		}
		slog.Error("get device config version", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to get config version")
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// ListVersions returns config version history for a device.
// @Summary List config version history
// @Tags device-config
// @Produce json
// @Param imei path string true "Device IMEI"
// @Param limit query int false "Max results" default(50)
// @Success 200 {array} store.DeviceConfig
// @Router /api/devices/{imei}/config/history [get]
func (h *DeviceConfigHandler) ListVersions(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	imei := chi.URLParam(r, "imei")

	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	configs, err := h.store.ListDeviceConfigVersions(r.Context(), tid, imei, limit)
	if err != nil {
		slog.Error("list device config versions", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list config versions")
		return
	}
	if configs == nil {
		configs = []store.DeviceConfig{}
	}
	writeJSON(w, http.StatusOK, configs)
}

// CreateVersion creates a new config version for a device.
// @Summary Create new config version
// @Tags device-config
// @Accept json
// @Produce json
// @Param imei path string true "Device IMEI"
// @Param body body createConfigRequest true "Config data"
// @Success 201 {object} store.DeviceConfig
// @Failure 400 {object} map[string]string
// @Router /api/devices/{imei}/config [put]
func (h *DeviceConfigHandler) CreateVersion(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	imei := chi.URLParam(r, "imei")

	var req createConfigRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if len(req.Config) == 0 {
		writeError(w, http.StatusBadRequest, "config is required")
		return
	}

	// Validate config is valid JSON.
	if !json.Valid(req.Config) {
		writeError(w, http.StatusBadRequest, "config must be valid JSON")
		return
	}

	// Get actor from auth context.
	actor := ""
	if u := auth.FromContext(r.Context()); u != nil {
		actor = u.ID
	}

	cfg := &store.DeviceConfig{
		DeviceIMEI: imei,
		Config:     string(req.Config),
		Author:     actor,
		Comment:    req.Comment,
	}

	if err := h.store.CreateDeviceConfig(r.Context(), tid, cfg); err != nil {
		slog.Error("create device config", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create config version")
		return
	}

	writeJSON(w, http.StatusCreated, cfg)
}
