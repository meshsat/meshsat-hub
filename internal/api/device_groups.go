package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// DeviceGroupHandler provides REST endpoints for device group management.
type DeviceGroupHandler struct {
	store store.Store
}

// NewDeviceGroupHandler creates a new device group API handler.
func NewDeviceGroupHandler(s store.Store) *DeviceGroupHandler {
	return &DeviceGroupHandler{store: s}
}

// ListGroups returns all device groups with member counts.
// @Summary List device groups
// @Tags device-groups
// @Produce json
// @Success 200 {array} store.DeviceGroup
// @Failure 500 {object} map[string]string
// @Router /api/device-groups [get]
func (h *DeviceGroupHandler) ListGroups(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	groups, err := h.store.ListDeviceGroups(r.Context(), tid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if groups == nil {
		groups = []store.DeviceGroup{}
	}
	writeJSON(w, http.StatusOK, groups)
}

// CreateGroup creates a new device group.
// @Summary Create device group
// @Tags device-groups
// @Accept json
// @Produce json
// @Param body body object true "Group data (name required)"
// @Success 201 {object} store.DeviceGroup
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/device-groups [post]
func (h *DeviceGroupHandler) CreateGroup(w http.ResponseWriter, r *http.Request) {
	var g store.DeviceGroup
	if err := readJSON(w, r, &g); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if g.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if g.Color == "" {
		g.Color = "#6b7280"
	}
	g.ID = fmt.Sprintf("grp-%d", time.Now().UnixNano())

	tid := auth.TenantIDFromContext(r.Context())
	if err := h.store.CreateDeviceGroup(r.Context(), tid, &g); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	created, _ := h.store.GetDeviceGroup(r.Context(), tid, g.ID)
	writeJSON(w, http.StatusCreated, created)
}

// GetGroup returns a single device group by ID.
// @Summary Get device group
// @Tags device-groups
// @Produce json
// @Param id path string true "Group ID"
// @Success 200 {object} store.DeviceGroup
// @Failure 404 {object} map[string]string
// @Router /api/device-groups/{id} [get]
func (h *DeviceGroupHandler) GetGroup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tid := auth.TenantIDFromContext(r.Context())
	g, err := h.store.GetDeviceGroup(r.Context(), tid, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "group not found")
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// UpdateGroup updates a device group's name, description, and color.
// @Summary Update device group
// @Tags device-groups
// @Accept json
// @Produce json
// @Param id path string true "Group ID"
// @Param body body object true "Fields to update (name, description, color)"
// @Success 200 {object} store.DeviceGroup
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/device-groups/{id} [put]
func (h *DeviceGroupHandler) UpdateGroup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tid := auth.TenantIDFromContext(r.Context())
	existing, err := h.store.GetDeviceGroup(r.Context(), tid, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "group not found")
		return
	}

	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Color       string `json:"color"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	existing.Name = req.Name
	existing.Description = req.Description
	if req.Color != "" {
		existing.Color = req.Color
	}
	if err := h.store.UpdateDeviceGroup(r.Context(), tid, existing); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	updated, _ := h.store.GetDeviceGroup(r.Context(), tid, id)
	writeJSON(w, http.StatusOK, updated)
}

// DeleteGroup deletes a device group and its member associations.
// @Summary Delete device group
// @Tags device-groups
// @Param id path string true "Group ID"
// @Success 204
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/device-groups/{id} [delete]
func (h *DeviceGroupHandler) DeleteGroup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tid := auth.TenantIDFromContext(r.Context())
	if _, err := h.store.GetDeviceGroup(r.Context(), tid, id); err != nil {
		writeError(w, http.StatusNotFound, "group not found")
		return
	}
	if err := h.store.DeleteDeviceGroup(r.Context(), tid, id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AddMember adds a device to a group.
// @Summary Add device to group
// @Tags device-groups
// @Accept json
// @Produce json
// @Param id path string true "Group ID"
// @Param body body object true "Device IMEI"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/device-groups/{id}/members [post]
func (h *DeviceGroupHandler) AddMember(w http.ResponseWriter, r *http.Request) {
	groupID := chi.URLParam(r, "id")
	tid := auth.TenantIDFromContext(r.Context())

	if _, err := h.store.GetDeviceGroup(r.Context(), tid, groupID); err != nil {
		writeError(w, http.StatusNotFound, "group not found")
		return
	}

	var req struct {
		IMEI string `json:"imei"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.IMEI == "" {
		writeError(w, http.StatusBadRequest, "imei is required")
		return
	}

	if err := h.store.AddDeviceToGroup(r.Context(), tid, groupID, req.IMEI); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// RemoveMember removes a device from a group.
// @Summary Remove device from group
// @Tags device-groups
// @Param id path string true "Group ID"
// @Param imei path string true "Device IMEI"
// @Success 204
// @Failure 500 {object} map[string]string
// @Router /api/device-groups/{id}/members/{imei} [delete]
func (h *DeviceGroupHandler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	groupID := chi.URLParam(r, "id")
	imei := chi.URLParam(r, "imei")
	tid := auth.TenantIDFromContext(r.Context())

	if err := h.store.RemoveDeviceFromGroup(r.Context(), tid, groupID, imei); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListDevices lists all devices in a group.
// @Summary List devices in group
// @Tags device-groups
// @Produce json
// @Param id path string true "Group ID"
// @Success 200 {array} store.Device
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/device-groups/{id}/devices [get]
func (h *DeviceGroupHandler) ListDevices(w http.ResponseWriter, r *http.Request) {
	groupID := chi.URLParam(r, "id")
	tid := auth.TenantIDFromContext(r.Context())

	if _, err := h.store.GetDeviceGroup(r.Context(), tid, groupID); err != nil {
		writeError(w, http.StatusNotFound, "group not found")
		return
	}

	devices, err := h.store.ListDevicesInGroup(r.Context(), tid, groupID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if devices == nil {
		devices = []store.Device{}
	}
	writeJSON(w, http.StatusOK, devices)
}
