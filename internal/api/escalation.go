package api

import (
	"log/slog"
	"net/http"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/escalation"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/go-chi/chi/v5"
)

// EscalationHandler provides REST endpoints for escalation chain and alert management.
type EscalationHandler struct {
	store  store.Store
	engine *escalation.Engine
}

// NewEscalationHandler creates a new escalation API handler.
func NewEscalationHandler(s store.Store, e *escalation.Engine) *EscalationHandler {
	return &EscalationHandler{store: s, engine: e}
}

// --- Escalation Chains ---

// ListChains returns all escalation chains for the tenant.
// @Summary List escalation chains
// @Tags escalation
// @Produce json
// @Success 200 {array} store.EscalationChain
// @Failure 500 {object} map[string]string
// @Router /api/escalation/chains [get]
func (h *EscalationHandler) ListChains(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	chains, err := h.store.ListEscalationChains(r.Context(), tid)
	if err != nil {
		slog.Error("list escalation chains", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list chains")
		return
	}
	if chains == nil {
		chains = []store.EscalationChain{}
	}
	writeJSON(w, http.StatusOK, chains)
}

// CreateChain creates a new escalation chain.
// @Summary Create escalation chain
// @Tags escalation
// @Accept json
// @Produce json
// @Param body body store.EscalationChain true "Chain configuration"
// @Success 201 {object} store.EscalationChain
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/escalation/chains [post]
func (h *EscalationHandler) CreateChain(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	var chain store.EscalationChain
	if err := readJSON(w, r, &chain); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if chain.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if len(chain.Tiers) == 0 {
		writeError(w, http.StatusBadRequest, "at least one tier is required")
		return
	}
	if err := h.store.CreateEscalationChain(r.Context(), tid, &chain); err != nil {
		slog.Error("create escalation chain", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create chain")
		return
	}
	writeJSON(w, http.StatusCreated, chain)
}

// GetChain returns a single escalation chain by ID.
// @Summary Get escalation chain
// @Tags escalation
// @Produce json
// @Param id path string true "Chain ID"
// @Success 200 {object} store.EscalationChain
// @Failure 404 {object} map[string]string
// @Router /api/escalation/chains/{id} [get]
func (h *EscalationHandler) GetChain(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	chain, err := h.store.GetEscalationChain(r.Context(), tid, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "chain not found")
		return
	}
	writeJSON(w, http.StatusOK, chain)
}

// DeleteChain deletes an escalation chain.
// @Summary Delete escalation chain
// @Tags escalation
// @Param id path string true "Chain ID"
// @Success 204
// @Failure 500 {object} map[string]string
// @Router /api/escalation/chains/{id} [delete]
func (h *EscalationHandler) DeleteChain(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	if err := h.store.DeleteEscalationChain(r.Context(), tid, id); err != nil {
		slog.Error("delete escalation chain", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete chain")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Alerts ---

// ListAlerts returns alerts for the tenant.
// @Summary List alerts
// @Tags escalation
// @Produce json
// @Param active query bool false "Only active alerts" default(false)
// @Param limit query int false "Max results" default(50)
// @Success 200 {array} store.Alert
// @Failure 500 {object} map[string]string
// @Router /api/alerts [get]
func (h *EscalationHandler) ListAlerts(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	activeOnly := r.URL.Query().Get("active") == "true"
	alerts, err := h.store.ListAlerts(r.Context(), tid, activeOnly, 50)
	if err != nil {
		slog.Error("list alerts", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list alerts")
		return
	}
	if alerts == nil {
		alerts = []store.Alert{}
	}
	writeJSON(w, http.StatusOK, alerts)
}

// GetAlert returns a single alert.
// @Summary Get alert
// @Tags escalation
// @Produce json
// @Param id path string true "Alert ID"
// @Success 200 {object} store.Alert
// @Failure 404 {object} map[string]string
// @Router /api/alerts/{id} [get]
func (h *EscalationHandler) GetAlert(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	alert, err := h.store.GetAlert(r.Context(), tid, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "alert not found")
		return
	}
	writeJSON(w, http.StatusOK, alert)
}

type triggerAlertRequest struct {
	ChainID    string `json:"chain_id"`
	DeviceIMEI string `json:"device_imei"`
	Type       string `json:"type"`
	Detail     string `json:"detail"`
}

// TriggerAlert creates a new alert and starts escalation.
// @Summary Trigger alert
// @Tags escalation
// @Accept json
// @Produce json
// @Param body body triggerAlertRequest true "Alert parameters"
// @Success 201 {object} store.Alert
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/alerts [post]
func (h *EscalationHandler) TriggerAlert(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	var req triggerAlertRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ChainID == "" {
		writeError(w, http.StatusBadRequest, "chain_id is required")
		return
	}
	if req.Type == "" {
		req.Type = "custom"
	}

	alert := &store.Alert{
		ChainID:    req.ChainID,
		DeviceIMEI: req.DeviceIMEI,
		Type:       req.Type,
		Detail:     req.Detail,
	}
	if err := h.engine.Trigger(r.Context(), tid, alert); err != nil {
		slog.Error("trigger alert", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to trigger alert")
		return
	}
	writeJSON(w, http.StatusCreated, alert)
}

type ackRequest struct {
	AckedBy string `json:"acked_by"`
}

// AcknowledgeAlert stops escalation for an alert.
// @Summary Acknowledge alert
// @Tags escalation
// @Accept json
// @Produce json
// @Param id path string true "Alert ID"
// @Param body body ackRequest true "Acknowledger"
// @Success 200 {object} store.Alert
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Router /api/alerts/{id}/ack [post]
func (h *EscalationHandler) AcknowledgeAlert(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")

	var req ackRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ackedBy := req.AckedBy
	if ackedBy == "" {
		if u := auth.FromContext(r.Context()); u != nil {
			ackedBy = u.ID
		} else {
			ackedBy = "unknown"
		}
	}

	if err := h.engine.Acknowledge(r.Context(), tid, id, ackedBy); err != nil {
		slog.Error("acknowledge alert", "error", err)
		writeError(w, http.StatusNotFound, "alert not found or already resolved")
		return
	}

	alert, _ := h.store.GetAlert(r.Context(), tid, id)
	writeJSON(w, http.StatusOK, alert)
}
