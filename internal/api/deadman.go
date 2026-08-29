package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/meshsat/meshsat-hub/internal/deadman"
)

// DeadmanHandler provides REST endpoints for dead man's switch management.
type DeadmanHandler struct {
	monitor *deadman.Monitor
}

// NewDeadmanHandler creates a new dead man's switch handler.
func NewDeadmanHandler(m *deadman.Monitor) *DeadmanHandler {
	return &DeadmanHandler{monitor: m}
}

type deadmanConfigRequest struct {
	ChainID     string `json:"chain_id"`
	IntervalMin int    `json:"interval_min"` // check-in interval in minutes
	GraceMin    int    `json:"grace_min"`    // grace period in minutes
	Enabled     bool   `json:"enabled"`
}

// ListConfigs returns all dead man's switch configs.
// @Summary List dead man's switch configs
// @Tags deadman
// @Produce json
// @Success 200 {array} deadman.Config
// @Router /api/deadman [get]
func (h *DeadmanHandler) ListConfigs(w http.ResponseWriter, _ *http.Request) {
	configs := h.monitor.ListConfigs()
	if configs == nil {
		configs = []deadman.Config{}
	}
	writeJSON(w, http.StatusOK, configs)
}

// Configure sets or updates dead man's switch for a device.
// @Summary Configure dead man's switch for device
// @Tags deadman
// @Accept json
// @Produce json
// @Param imei path string true "Device IMEI"
// @Param body body deadmanConfigRequest true "Dead man's switch configuration"
// @Success 200 {object} deadman.Config
// @Failure 400 {object} map[string]string
// @Router /api/deadman/{imei} [put]
func (h *DeadmanHandler) Configure(w http.ResponseWriter, r *http.Request) {
	imei := chi.URLParam(r, "imei")
	var req deadmanConfigRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ChainID == "" && req.Enabled {
		writeError(w, http.StatusBadRequest, "chain_id is required when enabled")
		return
	}
	if req.IntervalMin <= 0 {
		req.IntervalMin = 60 // default 1 hour
	}
	if req.GraceMin <= 0 {
		req.GraceMin = 10 // default 10 minutes
	}

	cfg := deadman.Config{
		DeviceIMEI: imei,
		ChainID:    req.ChainID,
		Interval:   time.Duration(req.IntervalMin) * time.Minute,
		Grace:      time.Duration(req.GraceMin) * time.Minute,
		Enabled:    req.Enabled,
	}
	h.monitor.Configure(cfg)
	writeJSON(w, http.StatusOK, cfg)
}

// Delete removes the dead man's switch for a device.
// @Summary Remove dead man's switch for device
// @Tags deadman
// @Param imei path string true "Device IMEI"
// @Success 204
// @Router /api/deadman/{imei} [delete]
func (h *DeadmanHandler) Delete(w http.ResponseWriter, r *http.Request) {
	imei := chi.URLParam(r, "imei")
	h.monitor.Remove(imei)
	w.WriteHeader(http.StatusNoContent)
}

type snoozeRequest struct {
	DurationMin int `json:"duration_min"`
}

// Snooze temporarily suppresses the dead man's switch for a device.
// @Summary Snooze dead man's switch
// @Tags deadman
// @Accept json
// @Produce json
// @Param imei path string true "Device IMEI"
// @Param body body snoozeRequest true "Snooze duration"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Router /api/deadman/{imei}/snooze [post]
func (h *DeadmanHandler) Snooze(w http.ResponseWriter, r *http.Request) {
	imei := chi.URLParam(r, "imei")
	var req snoozeRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.DurationMin <= 0 {
		req.DurationMin = 60
	}
	dur := time.Duration(req.DurationMin) * time.Minute
	h.monitor.Snooze(imei, dur)
	writeJSON(w, http.StatusOK, map[string]string{
		"status":   "snoozed",
		"device":   imei,
		"duration": dur.String(),
	})
}
