package api

import (
	"net/http"
	"strconv"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/go-chi/chi/v5"
)

// PositionHandler provides REST endpoints for device positions.
type PositionHandler struct {
	store store.Store
}

// NewPositionHandler creates a new position API handler.
func NewPositionHandler(s store.Store) *PositionHandler {
	return &PositionHandler{store: s}
}

// ListPositions returns position history for a device.
// GET /api/devices/{imei}/positions?limit={n}
func (h *PositionHandler) ListPositions(w http.ResponseWriter, r *http.Request) {
	imei := chi.URLParam(r, "imei")
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}

	tid := auth.TenantIDFromContext(r.Context())
	positions, err := h.store.ListPositions(r.Context(), tid, imei, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if positions == nil {
		positions = []store.Position{}
	}
	writeJSON(w, http.StatusOK, positions)
}

// LatestPosition returns the most recent position for a device.
// GET /api/devices/{imei}/position
func (h *PositionHandler) LatestPosition(w http.ResponseWriter, r *http.Request) {
	imei := chi.URLParam(r, "imei")
	tid := auth.TenantIDFromContext(r.Context())
	pos, err := h.store.LatestPosition(r.Context(), tid, imei)
	if err != nil {
		writeError(w, http.StatusNotFound, "no position data")
		return
	}
	writeJSON(w, http.StatusOK, pos)
}

// AllLatestPositions returns the latest position for ALL devices (for the map).
// GET /api/positions/latest
func (h *PositionHandler) AllLatestPositions(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	devices, err := h.store.ListDevices(r.Context(), tid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	type devicePosition struct {
		IMEI     string  `json:"imei"`
		Label    string  `json:"label"`
		Type     string  `json:"type"`
		Lat      float64 `json:"lat"`
		Lon      float64 `json:"lon"`
		Alt      float64 `json:"alt"`
		Source   string  `json:"source"`
		LastSeen string  `json:"last_seen"`
	}

	var positions []devicePosition
	for _, dev := range devices {
		pos, err := h.store.LatestPosition(r.Context(), tid, dev.IMEI)
		if err != nil {
			continue // device has no position data yet
		}
		positions = append(positions, devicePosition{
			IMEI:     dev.IMEI,
			Label:    dev.Label,
			Type:     dev.Type,
			Lat:      pos.Lat,
			Lon:      pos.Lon,
			Alt:      pos.Alt,
			Source:   pos.Source,
			LastSeen: pos.CreatedAt.Format("2006-01-02T15:04:05Z"),
		})
	}
	if positions == nil {
		positions = []devicePosition{}
	}
	writeJSON(w, http.StatusOK, positions)
}
