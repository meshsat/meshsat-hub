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
		if _, err := rand.Read(b); err != nil {
			writeError(w, http.StatusInternalServerError, "could not generate a geofence id")
			return
		}
		f.ID = "gf-" + hex.EncodeToString(b)
	}
	if f.Trigger == "" {
		f.Trigger = geo.TriggerBoth
	}
	// Persisted, not just added to a map (MESHSAT-1119). A fence that exists
	// only in memory is gone at the next rollout, and the customer has no way
	// to tell that from the Hub having forgotten it deliberately.
	if err := h.engine.Save(r.Context(), f); err != nil {
		writeError(w, http.StatusInternalServerError, "could not save the geofence")
		return
	}
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
	if _, err := h.engine.Delete(r.Context(), auth.TenantIDFromContext(r.Context()), id); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete the geofence")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
