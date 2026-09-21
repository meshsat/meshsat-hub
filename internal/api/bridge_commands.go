package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/bridge"
	"github.com/meshsat/meshsat-hub/internal/cmdjobs"
	"github.com/meshsat/meshsat-hub/internal/oob"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// BridgeCommandHandler provides REST endpoints for sending commands to bridges.
type BridgeCommandHandler struct {
	store     store.Store
	commander *bridge.Commander
	jobs      *cmdjobs.Store
}

// SetJobs gives the handler somewhere shared to keep commands that are in
// flight, which is what makes a command able to outlive its HTTP request and a
// re-sent POST harmless (MESHSAT-1279). Without it the endpoint behaves as it
// always did.
func (h *BridgeCommandHandler) SetJobs(j *cmdjobs.Store) { h.jobs = j }

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
	// RequestID makes the command idempotent: a second POST with the same id
	// sends nothing and is handed the first one's job. Optional; without it an
	// identical out-of-band command inside 90 s is treated the same way.
	RequestID string `json:"request_id,omitempty"`
	// Async answers 202 at once with the request id instead of holding the
	// connection until the bridge replies; poll GET
	// /api/bridges/{id}/commands/{request_id}. Use it for out-of-band legs: a
	// reply over SMS can take a minute and over satellite ten, and a request
	// idle that long does not survive the path in front of the Hub.
	Async bool `json:"async,omitempty"`
}

// commandAccepted is the 202 body: the command is running, poll for it.
type commandAccepted struct {
	RequestID string `json:"request_id"`
	Status    string `json:"status"` // "pending"
	Poll      string `json:"poll"`
}

// commandResponse is the response body for POST /api/bridges/{id}/command.
type commandResponse struct {
	RequestID string          `json:"request_id"`
	Status    string          `json:"status"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
	// Bearer is the leg the command went out on — "mqtt", "sms", "imt" or
	// "sbd". With via unset the commander picks it, so this is the only
	// place the caller learns what was chosen (MESHSAT-964 AC6).
	Bearer    string `json:"bearer,omitempty"`
	LatencyMs int64  `json:"latency_ms"`
}

// commandWriteBudget is how long a command request may hold its connection:
// the longest out-of-band reply wait a tenant may configure (HUB_OOB_TIMEOUT_MAX,
// 60 min) plus room to write the response.
const commandWriteBudget = 61 * time.Minute

// SendCommand sends a command to a bridge and waits for the response.
// @Summary Send command to bridge
// @Description Sends a command to a field bridge and waits for the response. Over MQTT: ping, flush_burst, send_text, send_mt, config_update, reboot, mgmt_*. With via sms|imt|sbd (or automatically while the bridge is MQTT-offline and paired): mgmt_ping, mgmt_status, mgmt_log, mgmt_reset, mgmt_bearer, mgmt_restart, reboot as sealed OOB frames.
// @Tags bridges
// @Accept json
// @Produce json
// @Param id path string true "Bridge ID"
// @Param body body commandRequest true "Command to send"
// @Success 200 {object} commandResponse
// @Success 202 {object} commandAccepted "Accepted: async was set, or this is a retry of a command still in flight"
// @Failure 400 {object} map[string]string "Unknown command, or arguments the command cannot take"
// @Failure 404 {object} map[string]string
// @Failure 409 {object} map[string]string "Bridge is offline"
// @Failure 502 {object} map[string]string "The bearer (Twilio, Cloudloop) refused the frame; the command did not leave"
// @Failure 504 {object} map[string]string "No reply within the bearer's wait; the command may still reach the bridge until its frame expires"
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
	reqID, explicitID := strings.TrimSpace(req.RequestID), true
	if reqID == "" {
		reqID, explicitID = uuid.NewString(), false
	}
	if len(reqID) > 80 {
		writeError(w, http.StatusBadRequest, "request_id is too long")
		return
	}
	cmd := protocol.Command{
		Cmd:          req.Cmd,
		RequestID:    reqID,
		TargetDevice: req.TargetDevice,
		Payload:      req.Payload,
	}

	// Claim the command before anything is sent. The edge proxy re-sends a POST
	// it believes went unanswered, and it believes that of any request idle for
	// 65 s: on 2026-09-20 one SMS ping with no answer went out four times. A
	// retry, or a double click, is handed the job that is already running. The
	// fast MQTT leg is claimed only when the caller asks for it with an id.
	outOfBand := via == "sms" || via == "imt" || via == "sbd" || (via == "" && !b.Online)
	var job *cmdjobs.Job
	if h.jobs != nil && (explicitID || outOfBand) {
		won, current, err := h.jobs.Begin(r.Context(), tid,
			cmdjobs.Job{RequestID: reqID, BridgeID: bridgeID, Cmd: req.Cmd, Via: via},
			explicitID, cmdjobs.Fingerprint(bridgeID, req.Cmd, via, req.Payload))
		switch {
		case err != nil:
			// The job store being down must not stop an operator reaching a
			// kit; it only costs the retry protection.
			slog.Warn("command: job store unavailable, sending without retry protection", "error", err)
		case !won && current != nil:
			h.writeJob(w, bridgeID, current)
			return
		case won:
			job = current
		}
	}

	if req.Async && job != nil {
		// The job outlives the request on purpose, so it must not die with it;
		// WithoutCancel keeps what the request carries (tenant, trace) and drops
		// only its cancellation. The budget is what ends it.
		jobCtx := context.WithoutCancel(r.Context())
		go func() {
			ctx, cancel := context.WithTimeout(jobCtx, commandWriteBudget)
			defer cancel()
			status, body, errText := h.run(ctx, tid, bridgeID, cmd, via, b.Online)
			if err := h.jobs.Finish(ctx, tid, job, status, body, errText); err != nil {
				slog.Error("command: could not record the outcome", "request_id", job.RequestID, "error", err)
			}
		}()
		h.writeJob(w, bridgeID, job)
		return
	}

	// The server's WriteTimeout is 15 s, sized for the API; a command over a
	// bearer waits for a kit (42 s measured over SMS, minutes over satellite),
	// and a reply that arrives after the deadline was written to a connection
	// the server had already cut, so the caller saw a 502 with no response
	// and no request log line (MESHSAT-1164, mgmt_log at 21 s). Extend the
	// deadline for this request only; the wrappers in the chain implement
	// Unwrap so the controller reaches the real writer.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(commandWriteBudget)); err != nil {
		slog.Warn("command: cannot extend the write deadline; a slow bearer reply will be lost", "error", err)
	}

	// A claimed command is not tied to this connection: if the caller goes
	// away, the command still completes and its result is there for the retry
	// or a poll to pick up.
	ctx := r.Context()
	if job != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(r.Context()), commandWriteBudget)
		defer cancel()
	}
	status, body, errText := h.run(ctx, tid, bridgeID, cmd, via, b.Online)
	if job != nil {
		if err := h.jobs.Finish(ctx, tid, job, status, body, errText); err != nil {
			slog.Error("command: could not record the outcome", "request_id", job.RequestID, "error", err)
		}
	}
	if status != http.StatusOK {
		writeError(w, status, errText)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// run sends the command and turns the outcome into what the API answers.
func (h *BridgeCommandHandler) run(ctx context.Context, tid, bridgeID string, cmd protocol.Command, via string, online bool) (int, *commandResponse, string) {
	start := time.Now()
	resp, err := h.commander.SendCommandVia(ctx, tid, bridgeID, cmd, via, online)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return http.StatusGatewayTimeout, nil, "timeout waiting for bridge response"
		case isTimeoutError(err):
			return http.StatusGatewayTimeout, nil, err.Error()
		// The caller's own mistake, with a message that says which: an unknown
		// command, or arguments the command cannot take. These came back as a
		// 500 "internal error", which told an operator nothing and looked like
		// a Hub fault in the log.
		case errors.Is(err, oob.ErrBadArgs) || errors.Is(err, oob.ErrUnknownCmd):
			return http.StatusBadRequest, nil, err.Error()
		// The provider refused the frame, so the command never left. Since
		// MESHSAT-1296 a Cloudloop refusal is an error rather than a "sent", and
		// its reason is the only useful thing to show; a 500 "internal error"
		// would hide it.
		case errors.Is(err, oob.ErrSendFailed):
			slog.Warn("command: the bearer refused it", "bridge", bridgeID, "cmd", cmd.Cmd, "via", via, "error", err)
			return http.StatusBadGateway, nil, err.Error()
		}
		slog.Error("command: failed", "bridge", bridgeID, "cmd", cmd.Cmd, "via", via, "error", err)
		return http.StatusInternalServerError, nil, "internal error"
	}
	return http.StatusOK, &commandResponse{
		RequestID: resp.RequestID,
		Status:    resp.Status,
		Result:    resp.Result,
		Error:     resp.Error,
		Bearer:    resp.Bearer,
		LatencyMs: latency,
	}, ""
}

// writeJob answers with a job: its outcome when it has one, else 202.
func (h *BridgeCommandHandler) writeJob(w http.ResponseWriter, bridgeID string, j *cmdjobs.Job) {
	switch j.State {
	case cmdjobs.StateDone:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(j.Response)
	case cmdjobs.StateFailed:
		writeError(w, j.HTTPStatus, j.Error)
	default:
		writeJSON(w, http.StatusAccepted, commandAccepted{
			RequestID: j.RequestID, Status: cmdjobs.StatePending,
			Poll: "/api/bridges/" + bridgeID + "/commands/" + j.RequestID,
		})
	}
}

// GetCommand returns a command that was sent with async, or whose POST was cut
// short, by its request id.
// @Summary Get the state of a bridge command
// @Description A command sent with async (or one whose connection was lost) keeps running on the Hub. This returns it: pending, or its outcome. Kept for two hours.
// @Tags bridges
// @Produce json
// @Param id path string true "Bridge ID"
// @Param request_id path string true "Request id returned by the POST"
// @Success 200 {object} cmdjobs.Job
// @Failure 404 {object} map[string]string
// @Router /api/bridges/{id}/commands/{request_id} [get]
func (h *BridgeCommandHandler) GetCommand(w http.ResponseWriter, r *http.Request) {
	if h.jobs == nil {
		writeError(w, http.StatusNotFound, "command not found")
		return
	}
	j, err := h.jobs.Get(r.Context(), auth.TenantIDFromContext(r.Context()), chi.URLParam(r, "id"), chi.URLParam(r, "request_id"))
	if err != nil {
		writeInternalError(w, err, "")
		return
	}
	if j == nil {
		writeError(w, http.StatusNotFound, "command not found")
		return
	}
	writeJSON(w, http.StatusOK, j)
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
