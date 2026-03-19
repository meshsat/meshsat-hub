package ipougrs

import (
	"encoding/json"
	"net/http"
)

// APIHandler provides REST endpoints for IPoUGRS tunnel management.
type APIHandler struct {
	tunnel *Tunnel
}

// NewAPIHandler creates an IPoUGRS API handler.
func NewAPIHandler(t *Tunnel) *APIHandler {
	return &APIHandler{tunnel: t}
}

// GetStatus returns the tunnel configuration and statistics.
//
//	@Summary      Get IPoUGRS tunnel status (experimental)
//	@Tags         ipougrs
//	@Produce      json
//	@Success      200  {object}  statusResponse
//	@Router       /api/ipougrs/status [get]
func (h *APIHandler) GetStatus(w http.ResponseWriter, _ *http.Request) {
	resp := statusResponse{
		Config: h.tunnel.GetConfig(),
		Stats:  h.tunnel.GetStats(),
		Alpha:  true,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

type statusResponse struct {
	Config Config `json:"config"`
	Stats  Stats  `json:"stats"`
	Alpha  bool   `json:"alpha"` // always true — experimental feature
}
