package api

import (
	"net/http"

	"github.com/cubeos-app/meshsat-hub/internal/reticulum"
)

// ReticulumTopologyHandler serves the full network topology view.
type ReticulumTopologyHandler struct {
	hubID     *reticulum.HubIdentity
	router    *reticulum.Router
	relay     *reticulum.Relay
	pathHdlr  *reticulum.PathHandler
	hintPub   *reticulum.RouteHintPublisher
}

// NewReticulumTopologyHandler creates a handler for /api/reticulum/topology.
func NewReticulumTopologyHandler(
	hubID *reticulum.HubIdentity,
	router *reticulum.Router,
	relay *reticulum.Relay,
	pathHdlr *reticulum.PathHandler,
	hintPub *reticulum.RouteHintPublisher,
) *ReticulumTopologyHandler {
	return &ReticulumTopologyHandler{
		hubID:    hubID,
		router:   router,
		relay:    relay,
		pathHdlr: pathHdlr,
		hintPub:  hintPub,
	}
}

// topologyResponse is the JSON response for GET /api/reticulum/topology.
type topologyResponse struct {
	Hub           topologyHub                        `json:"hub"`
	Routes        []reticulum.RouteInfo              `json:"routes"`
	Interfaces    []relayInterfaceInfo               `json:"interfaces"`
	RelayStats    reticulum.RelayStatsSnapshot       `json:"relay_stats"`
	PathStats     reticulum.PathHandlerStatsSnapshot  `json:"path_stats"`
	HintsPublished int64                             `json:"hints_published"`
}

type topologyHub struct {
	DestHash string `json:"dest_hash"`
	AppName  string `json:"app_name"`
	Role     string `json:"role"`
}

// GetTopology returns the full network topology with all stats.
// @Summary      Get Reticulum network topology
// @Description  Returns Hub identity, all known routes, interfaces, relay stats, and path discovery stats.
// @Tags         reticulum
// @Produce      json
// @Success      200  {object}  topologyResponse
// @Router       /api/reticulum/topology [get]
func (h *ReticulumTopologyHandler) GetTopology(w http.ResponseWriter, _ *http.Request) {
	hub := topologyHub{Role: "super_transport_node"}
	if h.hubID != nil && h.hubID.IsLoaded() {
		hub.DestHash = h.hubID.DestHashHex()
		hub.AppName = h.hubID.AppName()
	}

	var routes []reticulum.RouteInfo
	if h.router != nil {
		routes = h.router.AllRoutes()
	}

	var ifaces []relayInterfaceInfo
	var relayStats reticulum.RelayStatsSnapshot
	if h.relay != nil {
		for _, i := range h.relay.ListInterfaces() {
			ifaces = append(ifaces, relayInterfaceInfo{
				Name:      string(i.Name()),
				Cost:      i.Cost(),
				MTU:       i.MTU(),
				Available: i.IsAvailable(),
			})
		}
		relayStats = h.relay.Stats()
	}

	var pathStats reticulum.PathHandlerStatsSnapshot
	if h.pathHdlr != nil {
		pathStats = h.pathHdlr.Stats()
	}

	var hintsPublished int64
	if h.hintPub != nil {
		hintsPublished = h.hintPub.PublishedCount()
	}

	writeJSON(w, http.StatusOK, topologyResponse{
		Hub:            hub,
		Routes:         routes,
		Interfaces:     ifaces,
		RelayStats:     relayStats,
		PathStats:      pathStats,
		HintsPublished: hintsPublished,
	})
}
