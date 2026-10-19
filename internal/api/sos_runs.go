package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// SOSRunCanceller is an operator's cancel of an SOS run (internal/sos Runs, HeMB-NG C10).
type SOSRunCanceller interface {
	Cancel(ctx context.Context, tenantID, sosID, userID string) ([]store.SOSRun, error)
}

// SOSRunHandler serves the tenant's SOS runs: an SOS a sender raised under its own identity
// (HeMB-NG stage 2, C10, MESHSAT-1584), whichever bearers carried it.
type SOSRunHandler struct {
	store store.Store
	runs  SOSRunCanceller
}

// NewSOSRunHandler wires the handler; runs is the Hub's SOS run service.
func NewSOSRunHandler(st store.Store, runs SOSRunCanceller) *SOSRunHandler {
	return &SOSRunHandler{store: st, runs: runs}
}

// List returns the tenant's SOS runs, newest first.
// @Summary List SOS runs
// @Description Every SOS a bridge or phone raised under its own identity (C10): its state (active or cancelled), the bearers it arrived over, its alert, and who cancelled it.
// @Tags sos
// @Produce json
// @Param limit query int false "Max results" default(100)
// @Success 200 {array} store.SOSRun
// @Failure 500 {object} map[string]string
// @Router /api/sos/runs [get]
func (h *SOSRunHandler) List(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	limit := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	runs, err := h.store.ListSOSRuns(r.Context(), tid, limit)
	if err != nil {
		slog.Error("list sos runs", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list SOS runs")
		return
	}
	if runs == nil {
		runs = []store.SOSRun{}
	}
	writeJSON(w, http.StatusOK, runs)
}

// Cancel is an operator cancelling an SOS run on the Hub.
// @Summary Cancel an SOS run
// @Description Cancels the run for good: its alert stops escalating, and a start that arrives later pages nobody. Logged with who cancelled. The sender's own SOS on its device is not stopped by this.
// @Tags sos
// @Produce json
// @Param sos_id path string true "SOS id (UUID)"
// @Success 200 {array} store.SOSRun
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/sos/runs/{sos_id}/cancel [post]
func (h *SOSRunHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	sosID := chi.URLParam(r, "sos_id")
	userID := "unknown"
	if u := auth.FromContext(r.Context()); u != nil && u.ID != "" {
		userID = u.ID
	}
	runs, err := h.runs.Cancel(r.Context(), tid, sosID, userID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such SOS run")
		return
	}
	if err != nil {
		slog.Error("cancel sos run", "error", err, "sos_id", sosID)
		writeError(w, http.StatusInternalServerError, "failed to cancel the SOS run")
		return
	}
	writeJSON(w, http.StatusOK, runs)
}
