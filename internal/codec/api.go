package codec

import (
	"encoding/json"
	"net/http"
)

// APIHandler provides REST endpoints for codec management.
type APIHandler struct {
	registry *Registry
}

// NewAPIHandler creates a codec API handler.
func NewAPIHandler(r *Registry) *APIHandler {
	return &APIHandler{registry: r}
}

// ListCodecs returns all available decoders.
//
//	@Summary      List available payload decoders
//	@Tags         codecs
//	@Produce      json
//	@Success      200  {array}  CodecInfo
//	@Router       /api/codecs [get]
func (h *APIHandler) ListCodecs(w http.ResponseWriter, _ *http.Request) {
	codecs := h.registry.List()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(codecs)
}
