package ipougrs

import (
	"encoding/json"
	"net/http"
)

// StatusResponse is the API response for the IPoUGRS tunnel status endpoint.
type StatusResponse struct {
	Experimental bool   `json:"experimental"`
	Config       Config `json:"config"`
	Stats        Stats  `json:"stats"`
}

// APIHandler provides HTTP endpoints for the IPoUGRS tunnel.
type APIHandler struct {
	tunnel *Tunnel
}

// NewAPIHandler creates an API handler for the given tunnel.
func NewAPIHandler(tunnel *Tunnel) *APIHandler {
	return &APIHandler{tunnel: tunnel}
}

// GetStatus returns the current tunnel status.
// @Summary      Get IPoUGRS tunnel status
// @Description  Returns the configuration and statistics of the experimental IP-over-satellite tunnel.
// @Tags         ipougrs
// @Produce      json
// @Success      200  {object}  StatusResponse
// @Router       /api/ipougrs/status [get]
// @Security     BearerAuth
func (h *APIHandler) GetStatus(w http.ResponseWriter, _ *http.Request) {
	resp := StatusResponse{
		Experimental: true,
		Config:       h.tunnel.GetConfig(),
		Stats:        h.tunnel.GetStats(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
