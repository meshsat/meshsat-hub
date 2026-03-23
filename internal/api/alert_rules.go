package api

import (
	"log/slog"
	"net/http"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/go-chi/chi/v5"
)

// AlertRuleHandler provides REST endpoints for alert rule management.
type AlertRuleHandler struct {
	store store.Store
}

// NewAlertRuleHandler creates a new alert rule API handler.
func NewAlertRuleHandler(s store.Store) *AlertRuleHandler {
	return &AlertRuleHandler{store: s}
}

// ListAlertRules returns all alert rules for the tenant.
// @Summary List alert rules
// @Tags alerting
// @Produce json
// @Success 200 {array} store.AlertRule
// @Failure 500 {object} map[string]string
// @Router /api/alert-rules [get]
func (h *AlertRuleHandler) ListAlertRules(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	rules, err := h.store.ListAlertRules(r.Context(), tid)
	if err != nil {
		slog.Error("list alert rules", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list alert rules")
		return
	}
	if rules == nil {
		rules = []store.AlertRule{}
	}
	writeJSON(w, http.StatusOK, rules)
}

// CreateAlertRule creates a new alert rule.
// @Summary Create alert rule
// @Tags alerting
// @Accept json
// @Produce json
// @Param body body store.AlertRule true "Alert rule configuration"
// @Success 201 {object} store.AlertRule
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/alert-rules [post]
func (h *AlertRuleHandler) CreateAlertRule(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	var rule store.AlertRule
	if err := readJSON(w, r, &rule); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if rule.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if rule.ConditionType == "" {
		writeError(w, http.StatusBadRequest, "condition_type is required")
		return
	}
	if rule.ChainID == "" {
		writeError(w, http.StatusBadRequest, "chain_id is required")
		return
	}
	if rule.DeviceFilter == "" {
		rule.DeviceFilter = "*"
	}
	if rule.ConditionParams == "" {
		rule.ConditionParams = "{}"
	}
	if err := h.store.CreateAlertRule(r.Context(), tid, &rule); err != nil {
		slog.Error("create alert rule", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create alert rule")
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

// GetAlertRule returns a single alert rule by ID.
// @Summary Get alert rule
// @Tags alerting
// @Produce json
// @Param id path string true "Rule ID"
// @Success 200 {object} store.AlertRule
// @Failure 404 {object} map[string]string
// @Router /api/alert-rules/{id} [get]
func (h *AlertRuleHandler) GetAlertRule(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	rule, err := h.store.GetAlertRule(r.Context(), tid, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "alert rule not found")
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

// UpdateAlertRule updates an existing alert rule.
// @Summary Update alert rule
// @Tags alerting
// @Accept json
// @Produce json
// @Param id path string true "Rule ID"
// @Param body body store.AlertRule true "Updated alert rule"
// @Success 200 {object} store.AlertRule
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/alert-rules/{id} [put]
func (h *AlertRuleHandler) UpdateAlertRule(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")

	// Verify existence.
	existing, err := h.store.GetAlertRule(r.Context(), tid, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "alert rule not found")
		return
	}

	var rule store.AlertRule
	if err := readJSON(w, r, &rule); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rule.ID = existing.ID
	rule.CreatedAt = existing.CreatedAt
	if rule.DeviceFilter == "" {
		rule.DeviceFilter = "*"
	}
	if rule.ConditionParams == "" {
		rule.ConditionParams = "{}"
	}
	if err := h.store.UpdateAlertRule(r.Context(), tid, &rule); err != nil {
		slog.Error("update alert rule", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update alert rule")
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

// DeleteAlertRule deletes an alert rule.
// @Summary Delete alert rule
// @Tags alerting
// @Param id path string true "Rule ID"
// @Success 204
// @Failure 500 {object} map[string]string
// @Router /api/alert-rules/{id} [delete]
func (h *AlertRuleHandler) DeleteAlertRule(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	if err := h.store.DeleteAlertRule(r.Context(), tid, id); err != nil {
		slog.Error("delete alert rule", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete alert rule")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
