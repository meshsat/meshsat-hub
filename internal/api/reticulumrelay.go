package api

import (
	"net/http"

	"github.com/meshsat/meshsat-hub/internal/reticulum"
)

// ReticulumRelayHandler serves relay stats and interface info.
type ReticulumRelayHandler struct {
	relay *reticulum.Relay
}

// NewReticulumRelayHandler creates a handler for /api/reticulum/relay.
func NewReticulumRelayHandler(relay *reticulum.Relay) *ReticulumRelayHandler {
	return &ReticulumRelayHandler{relay: relay}
}

// relayStatusResponse is the JSON response for GET /api/reticulum/relay.
type relayStatusResponse struct {
	Stats      reticulum.RelayStatsSnapshot `json:"stats"`
	Interfaces []relayInterfaceInfo         `json:"interfaces"`
}

type relayInterfaceInfo struct {
	Name      string  `json:"name"`
	Cost      float64 `json:"cost"`
	MTU       int     `json:"mtu"`
	Available bool    `json:"available"`
}

// GetStatus returns relay stats and registered interfaces.
// @Summary      Get Reticulum relay status
// @Description  Returns packet forwarding stats and registered transport interfaces with availability.
// @Tags         reticulum
// @Produce      json
// @Success      200  {object}  relayStatusResponse
// @Router       /api/reticulum/relay [get]
func (h *ReticulumRelayHandler) GetStatus(w http.ResponseWriter, _ *http.Request) {
	if h.relay == nil {
		writeJSON(w, http.StatusOK, relayStatusResponse{})
		return
	}

	ifaces := h.relay.ListInterfaces()
	infos := make([]relayInterfaceInfo, 0, len(ifaces))
	for _, i := range ifaces {
		infos = append(infos, relayInterfaceInfo{
			Name:      string(i.Name()),
			Cost:      i.Cost(),
			MTU:       i.MTU(),
			Available: i.IsAvailable(),
		})
	}

	writeJSON(w, http.StatusOK, relayStatusResponse{
		Stats:      h.relay.Stats(),
		Interfaces: infos,
	})
}
