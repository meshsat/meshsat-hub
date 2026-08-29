package api

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// BridgeHandler provides REST endpoints for bridge management.
type BridgeHandler struct {
	store store.Store
	bus   bus.MessageBus
}

// NewBridgeHandler creates a new bridge API handler.
func NewBridgeHandler(s store.Store, mb bus.MessageBus) *BridgeHandler {
	return &BridgeHandler{store: s, bus: mb}
}

// bridgeCreateRequest is the request body for POST /api/bridges.
type bridgeCreateRequest struct {
	BridgeID string `json:"bridge_id"`
	Label    string `json:"label"`
	Location string `json:"location,omitempty"`
}

// CreateBridge manually pre-registers a bridge before it connects via MQTT.
// @Summary Pre-register a bridge
// @Description Creates a bridge record for manual provisioning. The bridge will appear as offline until it connects via MQTT.
// @Tags bridges
// @Accept json
// @Produce json
// @Param body body bridgeCreateRequest true "Bridge to create"
// @Success 201 {object} store.Bridge
// @Failure 400 {object} map[string]string
// @Failure 409 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/bridges [post]
func (h *BridgeHandler) CreateBridge(w http.ResponseWriter, r *http.Request) {
	var req bridgeCreateRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.BridgeID == "" {
		writeError(w, http.StatusBadRequest, "bridge_id is required")
		return
	}

	tid := auth.TenantIDFromContext(r.Context())

	// Check if bridge already exists.
	if existing, _ := h.store.GetBridge(r.Context(), tid, req.BridgeID); existing != nil {
		writeError(w, http.StatusConflict, "bridge already exists")
		return
	}

	b := &store.Bridge{
		BridgeID: req.BridgeID,
		TenantID: tid,
		Label:    req.Label,
	}
	if b.Label == "" {
		b.Label = req.BridgeID
	}

	if err := h.store.CreateOrUpdateBridge(r.Context(), tid, b); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	created, _ := h.store.GetBridge(r.Context(), tid, req.BridgeID)
	writeJSON(w, http.StatusCreated, created)
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

	// Clear retained MQTT messages so the bridge doesn't resurrect on subscriber reconnect.
	if h.bus != nil && h.bus.IsConnected() {
		for _, topic := range []string{
			protocol.TopicBridgeBirth(id),
			protocol.TopicBridgeDeath(id),
			protocol.TopicBridgeHealth(id),
		} {
			if err := h.bus.Publish(topic, 1, true, []byte{}); err != nil {
				slog.Warn("bridge delete: failed to clear retained message", "topic", topic, "error", err)
			}
		}
	}

	w.WriteHeader(http.StatusNoContent)
}
