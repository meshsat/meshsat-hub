package hawkbit

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// APIHandler provides HTTP endpoints for hawkBit OTA management.
type APIHandler struct {
	client *Client
}

// NewAPIHandler creates an API handler for hawkBit OTA operations.
func NewAPIHandler(client *Client) *APIHandler {
	return &APIHandler{client: client}
}

// ListTargets returns all OTA-managed devices.
// @Summary      List OTA targets
// @Tags         ota
// @Produce      json
// @Success      200  {object}  map[string][]Target
// @Failure      502  {object}  map[string]string
// @Router       /api/ota/targets [get]
func (h *APIHandler) ListTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := h.client.ListTargets(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"targets": targets})
}

// GetTarget returns a single OTA target.
// @Summary      Get OTA target
// @Tags         ota
// @Produce      json
// @Param        controllerId  path      string  true  "Target controller ID"
// @Success      200           {object}  Target
// @Failure      502           {object}  map[string]string
// @Router       /api/ota/targets/{controllerId} [get]
func (h *APIHandler) GetTarget(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "controllerId")
	target, err := h.client.GetTarget(r.Context(), id)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(target)
}

// CreateTarget registers a new device for OTA management.
// @Summary      Create OTA target
// @Tags         ota
// @Accept       json
// @Produce      json
// @Param        body  body      Target  true  "Target details"
// @Success      201   {object}  Target
// @Failure      400   {object}  map[string]string
// @Failure      502   {object}  map[string]string
// @Router       /api/ota/targets [post]
func (h *APIHandler) CreateTarget(w http.ResponseWriter, r *http.Request) {
	var target Target
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&target); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if target.ControllerId == "" {
		http.Error(w, `{"error":"controllerId is required"}`, http.StatusBadRequest)
		return
	}

	created, err := h.client.CreateTarget(r.Context(), target)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(created)
}

// DeleteTarget removes a device from OTA management.
// @Summary      Delete OTA target
// @Tags         ota
// @Param        controllerId  path  string  true  "Target controller ID"
// @Success      204
// @Failure      502  {object}  map[string]string
// @Router       /api/ota/targets/{controllerId} [delete]
func (h *APIHandler) DeleteTarget(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "controllerId")
	if err := h.client.DeleteTarget(r.Context(), id); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetTargetActions returns deployment actions for a target.
// @Summary      Get target deployment actions
// @Tags         ota
// @Produce      json
// @Param        controllerId  path      string  true  "Target controller ID"
// @Success      200           {object}  map[string][]ActionStatus
// @Failure      502           {object}  map[string]string
// @Router       /api/ota/targets/{controllerId}/actions [get]
func (h *APIHandler) GetTargetActions(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "controllerId")
	actions, err := h.client.GetTargetActions(r.Context(), id)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"actions": actions})
}

// CancelAction cancels a pending deployment action (rollback).
// @Summary      Cancel deployment action
// @Tags         ota
// @Param        controllerId  path  string  true  "Target controller ID"
// @Param        actionId      path  integer true  "Action ID"
// @Success      204
// @Failure      400  {object}  map[string]string
// @Failure      502  {object}  map[string]string
// @Router       /api/ota/targets/{controllerId}/actions/{actionId} [delete]
func (h *APIHandler) CancelAction(w http.ResponseWriter, r *http.Request) {
	controllerID := chi.URLParam(r, "controllerId")
	actionIDStr := chi.URLParam(r, "actionId")
	actionID, err := strconv.ParseInt(actionIDStr, 10, 64)
	if err != nil {
		http.Error(w, `{"error":"invalid action ID"}`, http.StatusBadRequest)
		return
	}

	if err := h.client.CancelAction(r.Context(), controllerID, actionID); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// CreateRollout starts a new firmware rollout campaign.
// @Summary      Create firmware rollout
// @Tags         ota
// @Accept       json
// @Produce      json
// @Param        body  body      Rollout  true  "Rollout details"
// @Success      201   {object}  Rollout
// @Failure      400   {object}  map[string]string
// @Failure      502   {object}  map[string]string
// @Router       /api/ota/rollouts [post]
func (h *APIHandler) CreateRollout(w http.ResponseWriter, r *http.Request) {
	var rollout Rollout
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&rollout); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if rollout.Name == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}

	created, err := h.client.CreateRollout(r.Context(), rollout)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(created)
}

// GetRollout returns rollout details.
// @Summary      Get rollout details
// @Tags         ota
// @Produce      json
// @Param        id   path      integer  true  "Rollout ID"
// @Success      200  {object}  Rollout
// @Failure      400  {object}  map[string]string
// @Failure      502  {object}  map[string]string
// @Router       /api/ota/rollouts/{id} [get]
func (h *APIHandler) GetRollout(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, `{"error":"invalid rollout ID"}`, http.StatusBadRequest)
		return
	}

	rollout, err := h.client.GetRollout(r.Context(), id)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rollout)
}

// StartRollout transitions a rollout to running.
// @Summary      Start a rollout
// @Tags         ota
// @Param        id  path  integer  true  "Rollout ID"
// @Success      204
// @Failure      400  {object}  map[string]string
// @Failure      502  {object}  map[string]string
// @Router       /api/ota/rollouts/{id}/start [post]
func (h *APIHandler) StartRollout(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, `{"error":"invalid rollout ID"}`, http.StatusBadRequest)
		return
	}

	if err := h.client.StartRollout(r.Context(), id); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PauseRollout pauses a running rollout.
// @Summary      Pause a rollout
// @Tags         ota
// @Param        id  path  integer  true  "Rollout ID"
// @Success      204
// @Failure      400  {object}  map[string]string
// @Failure      502  {object}  map[string]string
// @Router       /api/ota/rollouts/{id}/pause [post]
func (h *APIHandler) PauseRollout(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, `{"error":"invalid rollout ID"}`, http.StatusBadRequest)
		return
	}

	if err := h.client.PauseRollout(r.Context(), id); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
