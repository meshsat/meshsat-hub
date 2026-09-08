package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/bridge"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"
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
	// Via selects the leg: "" (MQTT while online, else the out-of-band
	// bearer the bridge is paired for), "mqtt", "sms", "imt" or "sbd"
	// (MESHSAT-964). Out-of-band legs carry mgmt_ping, mgmt_status,
	// mgmt_log, mgmt_reset, mgmt_bearer, mgmt_restart and reboot.
	Via string `json:"via,omitempty"`
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
// @Description Sends a command to a field bridge and waits for the response. Over MQTT: ping, flush_burst, send_text, send_mt, config_update, reboot, mgmt_*. With via sms|imt|sbd (or automatically while the bridge is MQTT-offline and paired): mgmt_ping, mgmt_status, mgmt_log, mgmt_reset, mgmt_bearer, mgmt_restart, reboot as sealed OOB frames.
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
	via := strings.ToLower(strings.TrimSpace(req.Via))
	switch via {
	case "", "mqtt", "sms", "imt", "sbd":
	default:
		writeError(w, http.StatusBadRequest, "via must be mqtt, sms, imt or sbd")
		return
	}
	// MQTT needs the bridge online; an out-of-band leg is for when it is not.
	if via == "mqtt" && !b.Online {
		writeError(w, http.StatusConflict, "bridge is offline")
		return
	}

	// Build protocol command.
	cmd := protocol.Command{
		Cmd:          req.Cmd,
		TargetDevice: req.TargetDevice,
		Payload:      req.Payload,
	}

	start := time.Now()
	resp, err := h.commander.SendCommandVia(r.Context(), tid, bridgeID, cmd, via, b.Online)
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
