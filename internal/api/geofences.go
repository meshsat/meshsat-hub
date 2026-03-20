package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/cubeos-app/meshsat-hub/internal/geo"
	"github.com/go-chi/chi/v5"
)

// GeofenceHandler provides CRUD for geofences via the in-memory geo.Engine.
type GeofenceHandler struct {
	engine *geo.Engine
}

// NewGeofenceHandler creates a geofence API handler.
func NewGeofenceHandler(engine *geo.Engine) *GeofenceHandler {
	return &GeofenceHandler{engine: engine}
}

// ListFences returns all configured geofences.
// @Summary      List geofences
// @Tags         geofences
// @Produce      json
// @Success      200  {array}  geo.Fence
// @Router       /api/geofences [get]
func (h *GeofenceHandler) ListFences(w http.ResponseWriter, _ *http.Request) {
	fences := h.engine.ListFences()
	writeJSON(w, http.StatusOK, fences)
}

// CreateFence adds a new geofence.
// @Summary      Create geofence
// @Tags         geofences
// @Accept       json
// @Produce      json
// @Success      201  {object}  geo.Fence
// @Router       /api/geofences [post]
func (h *GeofenceHandler) CreateFence(w http.ResponseWriter, r *http.Request) {
	var f geo.Fence
	if err := readJSON(w, r, &f); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(f.Polygon) < 3 {
		writeError(w, http.StatusBadRequest, "polygon must have at least 3 vertices")
		return
	}
	if f.ID == "" {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		f.ID = "gf-" + hex.EncodeToString(b)
	}
	if f.Trigger == "" {
		f.Trigger = geo.TriggerBoth
	}
	h.engine.AddFence(f)
	writeJSON(w, http.StatusCreated, f)
}

// DeleteFence removes a geofence by ID.
// @Summary      Delete geofence
// @Tags         geofences
// @Param        id   path  string  true  "Fence ID"
// @Success      204
// @Router       /api/geofences/{id} [delete]
func (h *GeofenceHandler) DeleteFence(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	h.engine.RemoveFence(id)
	w.WriteHeader(http.StatusNoContent)
}
