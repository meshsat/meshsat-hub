package routing

import (
	"log/slog"
	"net/http"

	"github.com/cubeos-app/meshsat-hub/internal/api"
	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/go-chi/chi/v5"
)

// APIHandler provides REST endpoints for route CRUD.
type APIHandler struct {
	store  store.Store
	engine *Engine
}

// NewAPIHandler creates a new routing API handler.
func NewAPIHandler(s store.Store, engine *Engine) *APIHandler {
	return &APIHandler{store: s, engine: engine}
}

// ListRoutes returns all routes for the tenant.
//
//	@Summary      List routing rules
//	@Tags         routing
//	@Produce      json
//	@Success      200  {array}  store.Route
//	@Router       /api/routes [get]
func (h *APIHandler) ListRoutes(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	routes, err := h.store.ListRoutes(r.Context(), tid)
	if err != nil {
		slog.Error("routing: list failed", "error", err)
		api.WriteError(w, http.StatusInternalServerError, "failed to list routes")
		return
	}
	if routes == nil {
		routes = []store.Route{}
	}
	api.WriteJSON(w, http.StatusOK, routes)
}

// GetRoute returns a single route.
//
//	@Summary      Get routing rule
//	@Tags         routing
//	@Produce      json
//	@Param        id  path  string  true  "Route ID"
//	@Success      200  {object}  store.Route
//	@Failure      404  {object}  map[string]string
//	@Router       /api/routes/{id} [get]
func (h *APIHandler) GetRoute(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")
	route, err := h.store.GetRoute(r.Context(), tid, id)
	if err != nil {
		api.WriteError(w, http.StatusNotFound, "route not found")
		return
	}
	api.WriteJSON(w, http.StatusOK, route)
}

type createRouteRequest struct {
	Name            string `json:"name"`
	SourceType      string `json:"source_type"`
	DestinationType string `json:"destination_type"`
	Filter          string `json:"filter,omitempty"`
	Enabled         *bool  `json:"enabled,omitempty"`
}

// CreateRoute creates a new routing rule.
//
//	@Summary      Create routing rule
//	@Tags         routing
//	@Accept       json
//	@Produce      json
//	@Param        body  body  createRouteRequest  true  "Route parameters"
//	@Success      201  {object}  store.Route
//	@Failure      400  {object}  map[string]string
//	@Router       /api/routes [post]
func (h *APIHandler) CreateRoute(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())

	var req createRouteRequest
	if err := api.ReadJSON(w, r, &req); err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.SourceType == "" || req.DestinationType == "" {
		api.WriteError(w, http.StatusBadRequest, "source_type and destination_type required")
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	route := &store.Route{
		Name:            req.Name,
		SourceType:      req.SourceType,
		DestinationType: req.DestinationType,
		Filter:          req.Filter,
		Enabled:         enabled,
	}

	if err := h.store.CreateRoute(r.Context(), tid, route); err != nil {
		slog.Error("routing: create failed", "error", err)
		api.WriteError(w, http.StatusInternalServerError, "failed to create route")
		return
	}

	h.engine.InvalidateCache()
	api.WriteJSON(w, http.StatusCreated, route)
}

// UpdateRoute updates an existing routing rule.
//
//	@Summary      Update routing rule
//	@Tags         routing
//	@Accept       json
//	@Produce      json
//	@Param        id    path  string            true  "Route ID"
//	@Param        body  body  createRouteRequest  true  "Route parameters"
//	@Success      200  {object}  store.Route
//	@Failure      400  {object}  map[string]string
//	@Failure      404  {object}  map[string]string
//	@Router       /api/routes/{id} [put]
func (h *APIHandler) UpdateRoute(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")

	existing, err := h.store.GetRoute(r.Context(), tid, id)
	if err != nil {
		api.WriteError(w, http.StatusNotFound, "route not found")
		return
	}

	var req createRouteRequest
	if err := api.ReadJSON(w, r, &req); err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.Name != "" {
		existing.Name = req.Name
	}
	if req.SourceType != "" {
		existing.SourceType = req.SourceType
	}
	if req.DestinationType != "" {
		existing.DestinationType = req.DestinationType
	}
	existing.Filter = req.Filter
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}

	if err := h.store.UpdateRoute(r.Context(), tid, existing); err != nil {
		slog.Error("routing: update failed", "error", err)
		api.WriteError(w, http.StatusInternalServerError, "failed to update route")
		return
	}

	h.engine.InvalidateCache()
	api.WriteJSON(w, http.StatusOK, existing)
}

// DeleteRoute removes a routing rule.
//
//	@Summary      Delete routing rule
//	@Tags         routing
//	@Param        id  path  string  true  "Route ID"
//	@Success      204
//	@Failure      404  {object}  map[string]string
//	@Router       /api/routes/{id} [delete]
func (h *APIHandler) DeleteRoute(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")

	if err := h.store.DeleteRoute(r.Context(), tid, id); err != nil {
		slog.Error("routing: delete failed", "error", err)
		api.WriteError(w, http.StatusInternalServerError, "failed to delete route")
		return
	}

	h.engine.InvalidateCache()
	w.WriteHeader(http.StatusNoContent)
}

type testRouteRequest struct {
	Channel  string `json:"channel"`
	DeviceID string `json:"device_id"`
	Text     string `json:"text"`
}

type testRouteResult struct {
	RouteID         string `json:"route_id"`
	RouteName       string `json:"route_name"`
	DestinationType string `json:"destination_type"`
	Matched         bool   `json:"matched"`
}

// TestRoutes evaluates all routes against a sample message and returns which matched.
//
//	@Summary      Test routing rules against sample message
//	@Tags         routing
//	@Accept       json
//	@Produce      json
//	@Param        body  body  testRouteRequest  true  "Sample message"
//	@Success      200  {array}  testRouteResult
//	@Failure      400  {object}  map[string]string
//	@Router       /api/routes/test [post]
func (h *APIHandler) TestRoutes(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())

	var req testRouteRequest
	if err := api.ReadJSON(w, r, &req); err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	routes, err := h.store.ListRoutes(r.Context(), tid)
	if err != nil {
		slog.Error("routing: test failed", "error", err)
		api.WriteError(w, http.StatusInternalServerError, "failed to list routes")
		return
	}

	sourceType := req.Channel
	if sourceType == "" {
		sourceType = "*"
	}

	results := make([]testRouteResult, 0, len(routes))
	for _, route := range routes {
		matched := route.Enabled &&
			matchSource(route.SourceType, sourceType) &&
			matchFilter(route.Filter, req.DeviceID, req.Text)
		results = append(results, testRouteResult{
			RouteID:         route.ID,
			RouteName:       route.Name,
			DestinationType: route.DestinationType,
			Matched:         matched,
		})
	}

	api.WriteJSON(w, http.StatusOK, results)
}
