package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/geo"
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

// paginatedPositions is the response envelope for paginated position queries.
type paginatedPositions struct {
	Positions []store.Position `json:"positions"`
	Total     int              `json:"total"`
	Limit     int              `json:"limit"`
	Offset    int              `json:"offset"`
}

// ListPositions returns paginated position history for a device.
// @Summary List device position history
// @Description Returns paginated position history with optional time range and track simplification.
// @Tags positions
// @Produce json
// @Param imei path string true "Device IMEI"
// @Param limit query int false "Max results per page" default(100)
// @Param offset query int false "Offset for pagination" default(0)
// @Param from query string false "Start time (RFC3339)"
// @Param to query string false "End time (RFC3339)"
// @Param simplify query number false "Douglas-Peucker epsilon in degrees (e.g. 0.0001 ≈ 11m)"
// @Success 200 {object} paginatedPositions
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/devices/{imei}/positions [get]
func (h *PositionHandler) ListPositions(w http.ResponseWriter, r *http.Request) {
	imei := chi.URLParam(r, "imei")
	q := r.URL.Query()

	limit := 100
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			if n > 10000 {
				n = 10000
			}
			limit = n
		}
	}

	offset := 0
	if o := q.Get("offset"); o != "" {
		if n, err := strconv.Atoi(o); err == nil && n >= 0 {
			offset = n
		}
	}

	var from, to time.Time
	if f := q.Get("from"); f != "" {
		if t, err := time.Parse(time.RFC3339, f); err == nil {
			from = t
		} else {
			writeError(w, http.StatusBadRequest, "invalid 'from' time format (use RFC3339)")
			return
		}
	}
	if t := q.Get("to"); t != "" {
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			to = parsed
		} else {
			writeError(w, http.StatusBadRequest, "invalid 'to' time format (use RFC3339)")
			return
		}
	}

	var epsilon float64
	if s := q.Get("simplify"); s != "" {
		if e, err := strconv.ParseFloat(s, 64); err == nil && e > 0 {
			epsilon = e
		}
	}

	tid := auth.TenantIDFromContext(r.Context())
	positions, total, err := h.store.ListPositionsRange(r.Context(), tid, imei, from, to, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if positions == nil {
		positions = []store.Position{}
	}

	// Apply Douglas-Peucker track simplification if requested.
	if epsilon > 0 && len(positions) > 2 {
		positions = simplifyTrack(positions, epsilon)
	}

	writeJSON(w, http.StatusOK, paginatedPositions{
		Positions: positions,
		Total:     total,
		Limit:     limit,
		Offset:    offset,
	})
}

// simplifyTrack applies Douglas-Peucker to a slice of positions.
// Positions are in descending time order from the DB; reverse for simplification
// (DP assumes sequential order), then reverse back.
func simplifyTrack(positions []store.Position, epsilon float64) []store.Position {
	n := len(positions)
	// Reverse to chronological order.
	points := make([]geo.Point, n)
	for i, p := range positions {
		points[n-1-i] = geo.Point{Lat: p.Lat, Lon: p.Lon}
	}

	simplified := geo.Simplify(points, epsilon)

	// Build a set of kept lat/lon pairs to filter the original positions.
	type key struct{ lat, lon float64 }
	keep := make(map[key]bool, len(simplified))
	for _, sp := range simplified {
		keep[key{sp.Lat, sp.Lon}] = true
	}

	result := make([]store.Position, 0, len(simplified))
	for _, p := range positions {
		if keep[key{p.Lat, p.Lon}] {
			result = append(result, p)
		}
	}
	return result
}

// LatestPosition returns the most recent position for a device.
// @Summary Get latest device position
// @Tags positions
// @Produce json
// @Param imei path string true "Device IMEI"
// @Success 200 {object} store.Position
// @Failure 404 {object} map[string]string
// @Router /api/devices/{imei}/position [get]
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
// @Summary Get latest positions for all devices
// @Tags positions
// @Produce json
// @Success 200 {array} object
// @Failure 500 {object} map[string]string
// @Router /api/positions/latest [get]
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
		Speed    float64 `json:"speed,omitempty"`
		Heading  float64 `json:"heading,omitempty"`
		Sats     int     `json:"sats,omitempty"`
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
			Speed:    pos.Speed,
			Heading:  pos.Heading,
			Sats:     pos.Sats,
			Source:   pos.Source,
			LastSeen: pos.CreatedAt.Format("2006-01-02T15:04:05Z"),
		})
	}
	if positions == nil {
		positions = []devicePosition{}
	}
	writeJSON(w, http.StatusOK, positions)
}
