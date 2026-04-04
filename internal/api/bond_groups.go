package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/bus"
	"github.com/cubeos-app/meshsat-hub/internal/protocol"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/go-chi/chi/v5"
)

// BondGroupHandler provides REST endpoints for HeMB bond group management.
type BondGroupHandler struct {
	store store.Store
	bus   bus.MessageBus
}

// NewBondGroupHandler creates a new bond group API handler.
func NewBondGroupHandler(s store.Store, mb bus.MessageBus) *BondGroupHandler {
	return &BondGroupHandler{store: s, bus: mb}
}

type createBondGroupRequest struct {
	Label      string   `json:"label"`
	Members    []string `json:"members"`
	CostBudget float64  `json:"cost_budget"`
}

type updateBondGroupRequest struct {
	Label      string   `json:"label"`
	Members    []string `json:"members"`
	CostBudget float64  `json:"cost_budget"`
}

// ListBondGroups returns all bond groups for a bridge.
// @Summary List bond groups for a bridge
// @Tags bond-groups
// @Produce json
// @Param bridgeID path string true "Bridge ID"
// @Success 200 {array} store.BondGroup
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/bridges/{bridgeID}/bond-groups [get]
func (h *BondGroupHandler) ListBondGroups(w http.ResponseWriter, r *http.Request) {
	bridgeID := chi.URLParam(r, "bridgeID")
	tid := auth.TenantIDFromContext(r.Context())

	if _, err := h.store.GetBridge(r.Context(), tid, bridgeID); err != nil {
		writeError(w, http.StatusNotFound, "bridge not found")
		return
	}

	groups, err := h.store.GetBondGroups(r.Context(), tid, bridgeID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if groups == nil {
		groups = []store.BondGroup{}
	}
	writeJSON(w, http.StatusOK, groups)
}

// CreateBondGroup creates a new bond group for a bridge.
// @Summary Create bond group
// @Tags bond-groups
// @Accept json
// @Produce json
// @Param bridgeID path string true "Bridge ID"
// @Param body body createBondGroupRequest true "Bond group data"
// @Success 201 {object} store.BondGroup
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/bridges/{bridgeID}/bond-groups [post]
func (h *BondGroupHandler) CreateBondGroup(w http.ResponseWriter, r *http.Request) {
	bridgeID := chi.URLParam(r, "bridgeID")
	tid := auth.TenantIDFromContext(r.Context())

	if _, err := h.store.GetBridge(r.Context(), tid, bridgeID); err != nil {
		writeError(w, http.StatusNotFound, "bridge not found")
		return
	}

	var req createBondGroupRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Label == "" {
		writeError(w, http.StatusBadRequest, "label is required")
		return
	}
	if req.CostBudget < 0 {
		writeError(w, http.StatusBadRequest, "cost_budget must be >= 0")
		return
	}

	membersJSON, _ := json.Marshal(req.Members)
	if req.Members == nil {
		membersJSON = []byte("[]")
	}

	g := &store.BondGroup{
		ID:         fmt.Sprintf("bg-%d", time.Now().UnixNano()),
		TenantID:   tid,
		BridgeID:   bridgeID,
		Label:      req.Label,
		Members:    string(membersJSON),
		CostBudget: req.CostBudget,
	}

	if err := h.store.CreateBondGroup(r.Context(), tid, bridgeID, g); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	created, _ := h.store.GetBondGroup(r.Context(), tid, bridgeID, g.ID)
	h.publishBondGroupConfig(r.Context(), tid, bridgeID)
	writeJSON(w, http.StatusCreated, created)
}

// GetBondGroup returns a single bond group by ID.
// @Summary Get bond group
// @Tags bond-groups
// @Produce json
// @Param bridgeID path string true "Bridge ID"
// @Param groupID path string true "Bond group ID"
// @Success 200 {object} store.BondGroup
// @Failure 404 {object} map[string]string
// @Router /api/bridges/{bridgeID}/bond-groups/{groupID} [get]
func (h *BondGroupHandler) GetBondGroup(w http.ResponseWriter, r *http.Request) {
	bridgeID := chi.URLParam(r, "bridgeID")
	groupID := chi.URLParam(r, "groupID")
	tid := auth.TenantIDFromContext(r.Context())

	g, err := h.store.GetBondGroup(r.Context(), tid, bridgeID, groupID)
	if err != nil {
		writeError(w, http.StatusNotFound, "bond group not found")
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// UpdateBondGroup updates a bond group's label, members, or cost budget.
// @Summary Update bond group
// @Tags bond-groups
// @Accept json
// @Produce json
// @Param bridgeID path string true "Bridge ID"
// @Param groupID path string true "Bond group ID"
// @Param body body updateBondGroupRequest true "Fields to update"
// @Success 200 {object} store.BondGroup
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/bridges/{bridgeID}/bond-groups/{groupID} [put]
func (h *BondGroupHandler) UpdateBondGroup(w http.ResponseWriter, r *http.Request) {
	bridgeID := chi.URLParam(r, "bridgeID")
	groupID := chi.URLParam(r, "groupID")
	tid := auth.TenantIDFromContext(r.Context())

	if _, err := h.store.GetBondGroup(r.Context(), tid, bridgeID, groupID); err != nil {
		writeError(w, http.StatusNotFound, "bond group not found")
		return
	}

	var req updateBondGroupRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.Label == "" {
		writeError(w, http.StatusBadRequest, "label is required")
		return
	}
	if req.CostBudget < 0 {
		writeError(w, http.StatusBadRequest, "cost_budget must be >= 0")
		return
	}

	membersJSON, _ := json.Marshal(req.Members)
	if req.Members == nil {
		membersJSON = []byte("[]")
	}

	g := &store.BondGroup{
		ID:         groupID,
		Label:      req.Label,
		Members:    string(membersJSON),
		CostBudget: req.CostBudget,
	}

	if err := h.store.UpdateBondGroup(r.Context(), tid, bridgeID, g); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	updated, _ := h.store.GetBondGroup(r.Context(), tid, bridgeID, groupID)
	h.publishBondGroupConfig(r.Context(), tid, bridgeID)
	writeJSON(w, http.StatusOK, updated)
}

// DeleteBondGroup removes a bond group.
// @Summary Delete bond group
// @Tags bond-groups
// @Param bridgeID path string true "Bridge ID"
// @Param groupID path string true "Bond group ID"
// @Success 204
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/bridges/{bridgeID}/bond-groups/{groupID} [delete]
func (h *BondGroupHandler) DeleteBondGroup(w http.ResponseWriter, r *http.Request) {
	bridgeID := chi.URLParam(r, "bridgeID")
	groupID := chi.URLParam(r, "groupID")
	tid := auth.TenantIDFromContext(r.Context())

	if _, err := h.store.GetBondGroup(r.Context(), tid, bridgeID, groupID); err != nil {
		writeError(w, http.StatusNotFound, "bond group not found")
		return
	}

	if err := h.store.DeleteBondGroup(r.Context(), tid, bridgeID, groupID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.publishBondGroupConfig(r.Context(), tid, bridgeID)
	w.WriteHeader(http.StatusNoContent)
}

// publishBondGroupConfig publishes the current bond group configuration to
// MQTT so the bridge can apply it. Best-effort: failures are logged but do
// not affect the HTTP response.
func (h *BondGroupHandler) publishBondGroupConfig(ctx context.Context, tenantID, bridgeID string) {
	if h.bus == nil || !h.bus.IsConnected() {
		return
	}
	groups, err := h.store.GetBondGroups(ctx, tenantID, bridgeID)
	if err != nil {
		slog.Warn("hemb: failed to fetch bond groups for MQTT push", "bridge", bridgeID, "error", err)
		return
	}
	if groups == nil {
		groups = []store.BondGroup{}
	}
	topic := protocol.TopicBridgeConfigHeMB(bridgeID)
	if err := h.bus.PublishJSON(topic, 1, true, groups); err != nil {
		slog.Warn("hemb: failed to publish bond group config", "bridge", bridgeID, "topic", topic, "error", err)
	}
}
