package ratelimit

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/auth"
)

// tenantOf is the tenant of the caller's session, which auth.TenantMiddleware
// puts on the context. Every endpoint below is scoped to it (MESHSAT-1118):
// reading usage used to show every tenant's devices, and setting an override
// used to exempt a device id belonging to anybody -- airtime billed to the
// victim's own carrier account.
func tenantOf(r *http.Request) string {
	return auth.TenantIDFromContext(r.Context())
}

// Handler provides REST API endpoints for rate limit management.
type Handler struct {
	limiter Limiter
}

// NewHandler creates a new rate limit API handler.
func NewHandler(limiter Limiter) *Handler {
	return &Handler{limiter: limiter}
}

// GetUsage returns rate limit usage for a specific device.
// @Summary Get rate limit usage for device
// @Tags ratelimit
// @Produce json
// @Param deviceID path string true "Device ID"
// @Success 200 {object} DeviceUsage
// @Failure 400 {object} map[string]string
// @Router /api/ratelimit/{deviceID} [get]
func (h *Handler) GetUsage(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	if deviceID == "" {
		http.Error(w, `{"error":"device ID required"}`, http.StatusBadRequest)
		return
	}
	usage := h.limiter.Usage(tenantOf(r), deviceID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(usage)
}

// GetAllUsage returns rate limit usage for all tracked devices.
// @Summary Get rate limit usage for all devices
// @Tags ratelimit
// @Produce json
// @Success 200 {array} DeviceUsage
// @Router /api/ratelimit [get]
func (h *Handler) GetAllUsage(w http.ResponseWriter, r *http.Request) {
	all := h.limiter.AllUsage(tenantOf(r))
	if all == nil {
		all = []DeviceUsage{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(all)
}

// PostOverride sets a temporary rate limit exemption.
// @Summary Set rate limit override for device
// @Tags ratelimit
// @Accept json
// @Produce json
// @Param deviceID path string true "Device ID"
// @Param body body object true "Override duration" example({"duration_hours": 24})
// @Success 201 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Router /api/ratelimit/{deviceID}/override [post]
func (h *Handler) PostOverride(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	if deviceID == "" {
		http.Error(w, `{"error":"device ID required"}`, http.StatusBadRequest)
		return
	}

	var req struct {
		DurationHours int `json:"duration_hours"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if req.DurationHours <= 0 {
		req.DurationHours = 24
	}

	SetOverride(tenantOf(r), deviceID, time.Duration(req.DurationHours)*time.Hour)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":   "override_set",
		"device":   deviceID,
		"duration": (time.Duration(req.DurationHours) * time.Hour).String(),
	})
}

// DeleteOverride removes a rate limit exemption.
// @Summary Remove rate limit override for device
// @Tags ratelimit
// @Param deviceID path string true "Device ID"
// @Success 204
// @Failure 400 {object} map[string]string
// @Router /api/ratelimit/{deviceID}/override [delete]
func (h *Handler) DeleteOverride(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	if deviceID == "" {
		http.Error(w, `{"error":"device ID required"}`, http.StatusBadRequest)
		return
	}
	ClearOverride(tenantOf(r), deviceID)
	w.WriteHeader(http.StatusNoContent)
}
