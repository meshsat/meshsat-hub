package ratelimit

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// Handler provides REST API endpoints for rate limit management.
type Handler struct {
	limiter *DeviceLimiter
}

// NewHandler creates a new rate limit API handler.
func NewHandler(limiter *DeviceLimiter) *Handler {
	return &Handler{limiter: limiter}
}

// GetUsage returns rate limit usage for a specific device.
// GET /api/ratelimit/{deviceID}
func (h *Handler) GetUsage(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	if deviceID == "" {
		http.Error(w, `{"error":"device ID required"}`, http.StatusBadRequest)
		return
	}
	usage := h.limiter.Usage(deviceID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(usage)
}

// GetAllUsage returns rate limit usage for all tracked devices.
// GET /api/ratelimit
func (h *Handler) GetAllUsage(w http.ResponseWriter, r *http.Request) {
	all := h.limiter.AllUsage()
	if all == nil {
		all = []DeviceUsage{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(all)
}

// PostOverride sets a temporary rate limit exemption.
// POST /api/ratelimit/{deviceID}/override
// Body: {"duration_hours": 24}
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

	SetOverride(deviceID, time.Duration(req.DurationHours)*time.Hour)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":   "override_set",
		"device":   deviceID,
		"duration": (time.Duration(req.DurationHours) * time.Hour).String(),
	})
}

// DeleteOverride removes a rate limit exemption.
// DELETE /api/ratelimit/{deviceID}/override
func (h *Handler) DeleteOverride(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	if deviceID == "" {
		http.Error(w, `{"error":"device ID required"}`, http.StatusBadRequest)
		return
	}
	ClearOverride(deviceID)
	w.WriteHeader(http.StatusNoContent)
}
