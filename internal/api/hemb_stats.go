package api

import (
	"net/http"

	"github.com/meshsat/meshsat-hub/internal/protocol"
)

// HeMBStatsProvider returns HeMB reassembly buffer statistics.
type HeMBStatsProvider interface {
	Stats() protocol.HeMBReassemblyStats
}

// HeMBStatsHandler exposes HeMB reassembly statistics via REST.
type HeMBStatsHandler struct {
	provider HeMBStatsProvider
}

// NewHeMBStatsHandler creates a new HeMB stats handler.
func NewHeMBStatsHandler(p HeMBStatsProvider) *HeMBStatsHandler {
	return &HeMBStatsHandler{provider: p}
}

// GetStats returns the current HeMB reassembly statistics.
// @Summary Get HeMB reassembly statistics
// @Tags hemb
// @Produce json
// @Success 200 {object} protocol.HeMBReassemblyStats
// @Router /api/hemb/stats [get]
func (h *HeMBStatsHandler) GetStats(w http.ResponseWriter, r *http.Request) {
	stats := h.provider.Stats()
	writeJSON(w, http.StatusOK, stats)
}
