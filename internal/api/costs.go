package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// CostsHandler provides REST endpoints for the cost tracking ledger.
type CostsHandler struct {
	store store.Store
}

// NewCostsHandler creates a new costs API handler.
func NewCostsHandler(s store.Store) *CostsHandler {
	return &CostsHandler{store: s}
}

// ListCosts returns cost ledger entries, optionally filtered by device and time range.
// @Summary List cost entries
// @Tags costs
// @Produce json
// @Param device query string false "Filter by device IMEI"
// @Param from query string false "Start time (RFC3339)"
// @Param to query string false "End time (RFC3339)"
// @Param limit query int false "Max results" default(100)
// @Success 200 {array} store.CostEntry
// @Failure 500 {object} map[string]string
// @Router /api/costs [get]
func (h *CostsHandler) ListCosts(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	device := r.URL.Query().Get("device")

	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}

	var from, to time.Time
	if f := r.URL.Query().Get("from"); f != "" {
		if t, err := time.Parse(time.RFC3339, f); err == nil {
			from = t
		}
	}
	if t := r.URL.Query().Get("to"); t != "" {
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			to = parsed
		}
	}

	entries, err := h.store.ListCostEntries(r.Context(), tid, device, from, to, limit)
	if err != nil {
		slog.Error("list cost entries", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list cost entries")
		return
	}
	if entries == nil {
		entries = []store.CostEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// Summary returns aggregated cost data grouped by device or month.
// @Summary Aggregate costs
// @Tags costs
// @Produce json
// @Param group_by query string false "Group by: device or month" default(device)
// @Param from query string false "Start time (RFC3339)"
// @Param to query string false "End time (RFC3339)"
// @Success 200 {array} store.CostAggregate
// @Failure 500 {object} map[string]string
// @Router /api/costs/summary [get]
func (h *CostsHandler) Summary(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	groupBy := r.URL.Query().Get("group_by")
	if groupBy == "" {
		groupBy = "device"
	}

	var from, to time.Time
	if f := r.URL.Query().Get("from"); f != "" {
		if t, err := time.Parse(time.RFC3339, f); err == nil {
			from = t
		}
	}
	if t := r.URL.Query().Get("to"); t != "" {
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			to = parsed
		}
	}

	aggs, err := h.store.AggregateCosts(r.Context(), tid, from, to, groupBy)
	if err != nil {
		slog.Error("aggregate costs", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to aggregate costs")
		return
	}
	if aggs == nil {
		aggs = []store.CostAggregate{}
	}
	writeJSON(w, http.StatusOK, aggs)
}
