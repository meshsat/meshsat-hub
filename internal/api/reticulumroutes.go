package api

import (
	"net/http"

	"github.com/meshsat/meshsat-hub/internal/reticulum"
)

// ReticulumRoutesHandler serves routing table info.
type ReticulumRoutesHandler struct {
	router *reticulum.Router
}

// NewReticulumRoutesHandler creates a handler for /api/reticulum/routes.
func NewReticulumRoutesHandler(router *reticulum.Router) *ReticulumRoutesHandler {
	return &ReticulumRoutesHandler{router: router}
}

// routeTableResponse is the JSON response for GET /api/reticulum/routes.
type routeTableResponse struct {
	Count  int                   `json:"count"`
	Routes []reticulum.RouteInfo `json:"routes"`
}

// ListRoutes returns all known Reticulum routes.
// @Summary      List Reticulum routes
// @Description  Returns all non-expired entries in the Reticulum routing table with interface, cost, and hop count.
// @Tags         reticulum
// @Produce      json
// @Success      200  {object}  routeTableResponse
// @Router       /api/reticulum/routes [get]
func (h *ReticulumRoutesHandler) ListRoutes(w http.ResponseWriter, _ *http.Request) {
	routes := h.router.AllRoutes()
	writeJSON(w, http.StatusOK, routeTableResponse{
		Count:  len(routes),
		Routes: routes,
	})
}
