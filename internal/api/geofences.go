package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/geo"
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
func (h *GeofenceHandler) ListFences(w http.ResponseWriter, r *http.Request) {
	// The tenant comes from auth.TenantMiddleware, which every /api route is
	// behind. A fence polygon says where a customer operates; this used to
	// return every tenant's (MESHSAT-1118).
	fences := h.engine.ListFences(auth.TenantIDFromContext(r.Context()))
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
	// The session decides the owner, never the body. geo.Fence has carried a
	// tenant_id JSON field all along, so a caller could always send one; until
	// now nothing set it and nothing read it.
	f.TenantID = auth.TenantIDFromContext(r.Context())
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
	// Scoped to the caller's tenant. 204 either way: whether another tenant
	// happens to hold that id is not this caller's business.
	h.engine.RemoveFence(auth.TenantIDFromContext(r.Context()), id)
	w.WriteHeader(http.StatusNoContent)
}
