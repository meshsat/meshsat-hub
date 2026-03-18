package api

import (
	"log/slog"
	"net/http"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/go-chi/chi/v5"
)

// NotificationHandler provides REST endpoints for per-device notification preferences.
type NotificationHandler struct {
	store store.Store
}

// NewNotificationHandler creates a new notification preferences handler.
func NewNotificationHandler(s store.Store) *NotificationHandler {
	return &NotificationHandler{store: s}
}

// ListPrefs returns all notification preferences for the tenant.
// @Summary List notification preferences
// @Tags notifications
// @Produce json
// @Success 200 {array} store.NotificationPref
// @Failure 500 {object} map[string]string
// @Router /api/notifications/prefs [get]
func (h *NotificationHandler) ListPrefs(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	prefs, err := h.store.ListNotificationPrefs(r.Context(), tid)
	if err != nil {
		slog.Error("list notification prefs", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list notification prefs")
		return
	}
	if prefs == nil {
		prefs = []store.NotificationPref{}
	}
	writeJSON(w, http.StatusOK, prefs)
}

// GetPref returns notification preferences for a specific device (or "*" for tenant default).
// @Summary Get notification preferences for a device
// @Tags notifications
// @Produce json
// @Param device_imei path string true "Device IMEI or * for tenant default"
// @Success 200 {object} store.NotificationPref
// @Failure 404 {object} map[string]string
// @Router /api/notifications/prefs/{device_imei} [get]
func (h *NotificationHandler) GetPref(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	deviceIMEI := chi.URLParam(r, "device_imei")
	pref, err := h.store.GetNotificationPref(r.Context(), tid, deviceIMEI)
	if err != nil {
		writeError(w, http.StatusNotFound, "notification pref not found")
		return
	}
	writeJSON(w, http.StatusOK, pref)
}

// SavePref creates or updates notification preferences for a device.
// @Summary Save notification preferences for a device
// @Tags notifications
// @Accept json
// @Produce json
// @Param device_imei path string true "Device IMEI or * for tenant default"
// @Param body body store.NotificationPref true "Notification preferences"
// @Success 200 {object} store.NotificationPref
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/notifications/prefs/{device_imei} [put]
func (h *NotificationHandler) SavePref(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	deviceIMEI := chi.URLParam(r, "device_imei")

	var pref store.NotificationPref
	if err := readJSON(w, r, &pref); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(pref.URLs) == 0 {
		writeError(w, http.StatusBadRequest, "urls is required (at least one Apprise URL)")
		return
	}
	pref.DeviceIMEI = deviceIMEI

	if err := h.store.SaveNotificationPref(r.Context(), tid, &pref); err != nil {
		slog.Error("save notification pref", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to save notification pref")
		return
	}

	saved, err := h.store.GetNotificationPref(r.Context(), tid, deviceIMEI)
	if err != nil {
		writeJSON(w, http.StatusOK, pref)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// DeletePref removes notification preferences for a device.
// @Summary Delete notification preferences for a device
// @Tags notifications
// @Param device_imei path string true "Device IMEI or * for tenant default"
// @Success 204
// @Failure 500 {object} map[string]string
// @Router /api/notifications/prefs/{device_imei} [delete]
func (h *NotificationHandler) DeletePref(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	deviceIMEI := chi.URLParam(r, "device_imei")
	if err := h.store.DeleteNotificationPref(r.Context(), tid, deviceIMEI); err != nil {
		slog.Error("delete notification pref", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete notification pref")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
