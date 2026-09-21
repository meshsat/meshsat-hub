package routing

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/httpjson"
	"github.com/meshsat/meshsat-hub/internal/store"
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
		httpjson.WriteError(w, http.StatusInternalServerError, "failed to list routes")
		return
	}
	if routes == nil {
		routes = []store.Route{}
	}
	httpjson.WriteJSON(w, http.StatusOK, routes)
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
		httpjson.WriteError(w, http.StatusNotFound, "route not found")
		return
	}
	httpjson.WriteJSON(w, http.StatusOK, route)
}

type createRouteRequest struct {
	Name            string `json:"name"`
	SourceType      string `json:"source_type"`
	DestinationType string `json:"destination_type"`
	Filter          string `json:"filter,omitempty"`
	Senders         string `json:"senders,omitempty"` // comma-separated origins; empty = any (MESHSAT-964)
	Enabled         *bool  `json:"enabled,omitempty"`
}

// updateRouteRequest is createRouteRequest with Filter and Senders as POINTERS:
// absent leaves the field alone, "" clears it. They were plain strings and the
// handler assigned them unconditionally, so every other field behaved as a
// partial update while these two were silently blanked by any request that did
// not repeat them. For a recipient destination the filter IS the recipient list
// and senders is who may trigger the route: renaming the two kit-to-kit SMS
// routes on 21 Sep 2026 left them with nobody to send to and open to any
// sender, and nothing said so. Same rule as the tenant settings: a plain field
// makes every unrelated save reset the setting.
type updateRouteRequest struct {
	Name            string  `json:"name"`
	SourceType      string  `json:"source_type"`
	DestinationType string  `json:"destination_type"`
	Filter          *string `json:"filter,omitempty"`
	Senders         *string `json:"senders,omitempty"`
	Enabled         *bool   `json:"enabled,omitempty"`
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
	if err := httpjson.ReadJSON(w, r, &req); err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.SourceType == "" || req.DestinationType == "" {
		httpjson.WriteError(w, http.StatusBadRequest, "source_type and destination_type required")
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
		Senders:         req.Senders,
		Enabled:         enabled,
	}

	if err := h.store.CreateRoute(r.Context(), tid, route); err != nil {
		slog.Error("routing: create failed", "error", err)
		httpjson.WriteError(w, http.StatusInternalServerError, "failed to create route")
		return
	}

	h.engine.InvalidateCache()
	httpjson.WriteJSON(w, http.StatusCreated, route)
}

// UpdateRoute updates an existing routing rule.
//
//	@Summary      Update routing rule
//	@Tags         routing
//	@Accept       json
//	@Produce      json
//	@Param        id    path  string            true  "Route ID"
//	@Param        body  body  updateRouteRequest  true  "Fields to change; an absent field is left as it is"
//	@Success      200  {object}  store.Route
//	@Failure      400  {object}  map[string]string
//	@Failure      404  {object}  map[string]string
//	@Router       /api/routes/{id} [put]
func (h *APIHandler) UpdateRoute(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")

	existing, err := h.store.GetRoute(r.Context(), tid, id)
	if err != nil {
		httpjson.WriteError(w, http.StatusNotFound, "route not found")
		return
	}

	var req updateRouteRequest
	if err := httpjson.ReadJSON(w, r, &req); err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, err.Error())
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
	if req.Filter != nil {
		existing.Filter = *req.Filter
	}
	if req.Senders != nil {
		existing.Senders = *req.Senders
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}

	if err := h.store.UpdateRoute(r.Context(), tid, existing); err != nil {
		slog.Error("routing: update failed", "error", err)
		httpjson.WriteError(w, http.StatusInternalServerError, "failed to update route")
		return
	}

	h.engine.InvalidateCache()
	httpjson.WriteJSON(w, http.StatusOK, existing)
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
		httpjson.WriteError(w, http.StatusInternalServerError, "failed to delete route")
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
	if err := httpjson.ReadJSON(w, r, &req); err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	routes, err := h.store.ListRoutes(r.Context(), tid)
	if err != nil {
		slog.Error("routing: test failed", "error", err)
		httpjson.WriteError(w, http.StatusInternalServerError, "failed to list routes")
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

	httpjson.WriteJSON(w, http.StatusOK, results)
}
