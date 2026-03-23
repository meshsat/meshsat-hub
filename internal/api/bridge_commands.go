package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/bridge"
	"github.com/cubeos-app/meshsat-hub/internal/protocol"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/go-chi/chi/v5"
)

// BridgeCommandHandler provides REST endpoints for sending commands to bridges.
type BridgeCommandHandler struct {
	store     store.Store
	commander *bridge.Commander
}

// NewBridgeCommandHandler creates a new bridge command API handler.
func NewBridgeCommandHandler(s store.Store, cmdr *bridge.Commander) *BridgeCommandHandler {
	return &BridgeCommandHandler{store: s, commander: cmdr}
}

// commandRequest is the request body for POST /api/bridges/{id}/command.
type commandRequest struct {
	Cmd          string          `json:"cmd"`
	TargetDevice string          `json:"target_device,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty"`
}

// commandResponse is the response body for POST /api/bridges/{id}/command.
type commandResponse struct {
	RequestID string          `json:"request_id"`
	Status    string          `json:"status"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
	LatencyMs int64           `json:"latency_ms"`
}

// SendCommand sends a command to a bridge and waits for the response.
// @Summary Send command to bridge
// @Description Sends a command to a field bridge via MQTT and waits for the response. Supported commands: ping, flush_burst, send_text, send_mt, config_update, reboot.
// @Tags bridges
// @Accept json
// @Produce json
// @Param id path string true "Bridge ID"
// @Param body body commandRequest true "Command to send"
// @Success 200 {object} commandResponse
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 409 {object} map[string]string "Bridge is offline"
// @Failure 504 {object} map[string]string "Timeout waiting for bridge response"
// @Router /api/bridges/{id}/command [post]
func (h *BridgeCommandHandler) SendCommand(w http.ResponseWriter, r *http.Request) {
	bridgeID := chi.URLParam(r, "id")
	tid := auth.TenantIDFromContext(r.Context())

	// Verify bridge exists.
	b, err := h.store.GetBridge(r.Context(), tid, bridgeID)
	if err != nil || b == nil {
		writeError(w, http.StatusNotFound, "bridge not found")
		return
	}

	// Check bridge is online.
	if !b.Online {
		writeError(w, http.StatusConflict, "bridge is offline")
		return
	}

	// Parse request body.
	var req commandRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Cmd == "" {
		writeError(w, http.StatusBadRequest, "cmd is required")
		return
	}

	// Build protocol command.
	cmd := protocol.Command{
		Cmd:          req.Cmd,
		TargetDevice: req.TargetDevice,
		Payload:      req.Payload,
	}

	start := time.Now()
	resp, err := h.commander.SendCommand(r.Context(), bridgeID, cmd)
	latency := time.Since(start).Milliseconds()

	if err != nil {
		// Distinguish timeout from other errors.
		if r.Context().Err() != nil {
			writeError(w, http.StatusGatewayTimeout, "timeout waiting for bridge response")
			return
		}
		// Check if it's a timeout error from the commander.
		if isTimeoutError(err) {
			writeError(w, http.StatusGatewayTimeout, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, commandResponse{
		RequestID: resp.RequestID,
		Status:    resp.Status,
		Result:    resp.Result,
		Error:     resp.Error,
		LatencyMs: latency,
	})
}

// isTimeoutError checks if an error message indicates a timeout.
func isTimeoutError(err error) bool {
	msg := err.Error()
	for i := 0; i <= len(msg)-7; i++ {
		if msg[i:i+7] == "timeout" {
			return true
		}
	}
	return false
}
