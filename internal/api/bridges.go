package api

import (
	"net/http"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/go-chi/chi/v5"
)

// BridgeHandler provides REST endpoints for bridge management.
type BridgeHandler struct {
	store store.Store
}

// NewBridgeHandler creates a new bridge API handler.
func NewBridgeHandler(s store.Store) *BridgeHandler {
	return &BridgeHandler{store: s}
}

// ListBridges returns all registered bridges for the tenant.
// @Summary List bridges
// @Tags bridges
// @Produce json
// @Success 200 {array} store.Bridge
// @Failure 500 {object} map[string]string
// @Router /api/bridges [get]
func (h *BridgeHandler) ListBridges(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	bridges, err := h.store.ListBridges(r.Context(), tid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if bridges == nil {
		bridges = []*store.Bridge{}
	}
	writeJSON(w, http.StatusOK, bridges)
}

// GetBridge returns a single bridge by ID.
// @Summary Get bridge by ID
// @Tags bridges
// @Produce json
// @Param id path string true "Bridge ID"
// @Success 200 {object} store.Bridge
// @Failure 404 {object} map[string]string
// @Router /api/bridges/{id} [get]
func (h *BridgeHandler) GetBridge(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tid := auth.TenantIDFromContext(r.Context())
	bridge, err := h.store.GetBridge(r.Context(), tid, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "bridge not found")
		return
	}
	writeJSON(w, http.StatusOK, bridge)
}

// UpdateBridge updates a bridge's label and/or CoT callsign.
// @Summary Update bridge
// @Tags bridges
// @Accept json
// @Produce json
// @Param id path string true "Bridge ID"
// @Param body body store.BridgeUpdate true "Fields to update"
// @Success 200 {object} store.Bridge
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/bridges/{id} [put]
func (h *BridgeHandler) UpdateBridge(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tid := auth.TenantIDFromContext(r.Context())

	// Verify bridge exists.
	if _, err := h.store.GetBridge(r.Context(), tid, id); err != nil {
		writeError(w, http.StatusNotFound, "bridge not found")
		return
	}

	var req store.BridgeUpdate
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := h.store.UpdateBridge(r.Context(), tid, id, req); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	updated, _ := h.store.GetBridge(r.Context(), tid, id)
	writeJSON(w, http.StatusOK, updated)
}

// DeleteBridge removes a bridge and disassociates its devices.
// @Summary Delete bridge
// @Tags bridges
// @Param id path string true "Bridge ID"
// @Success 204
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/bridges/{id} [delete]
func (h *BridgeHandler) DeleteBridge(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tid := auth.TenantIDFromContext(r.Context())

	if _, err := h.store.GetBridge(r.Context(), tid, id); err != nil {
		writeError(w, http.StatusNotFound, "bridge not found")
		return
	}

	if err := h.store.DeleteBridge(r.Context(), tid, id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
