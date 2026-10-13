package api

import (
	"net/http"

	"github.com/meshsat/meshsat-hub/internal/rnsnode"
)

// RNSHandler is the platform admin's read-only view of the Hub's
// upstream-compatible Reticulum node (MESHSAT-1555): whether this replica
// runs it, its listeners and connections, and the path table. Every route
// is platform-admin only; a path table names every peer that ever announced
// through the Hub, which is operator information, not a tenant's.
type RNSHandler struct {
	svc *rnsnode.Service // nil when the node is configured off
}

// NewRNSHandler builds the handler; svc may be nil.
func NewRNSHandler(svc *rnsnode.Service) *RNSHandler { return &RNSHandler{svc: svc} }

// Status answers for THIS replica. Only the leader is active, so the two
// replicas answer differently; read both when verifying a rollout.
//
//	@Summary      Reticulum node status
//	@Description  Whether this replica runs the upstream-compatible Reticulum node, its TCP listeners and their connections, the path and link counts and the node's counters. Only the leader is active.
//	@Tags         rns
//	@Produce      json
//	@Success      200  {object}  rnsnode.Status
//	@Router       /api/rns/status [get]
func (h *RNSHandler) Status(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeJSON(w, http.StatusOK, rnsnode.Status{})
		return
	}
	writeJSON(w, http.StatusOK, h.svc.Status())
}

// Paths lists the node's path table on this replica (empty when inactive).
//
//	@Summary      Reticulum path table
//	@Description  Every destination the node knows a path to, with next hop, hop count, interface and expiry. Empty on a replica that is not the leader.
//	@Tags         rns
//	@Produce      json
//	@Success      200  {array}   rnsnode.PathInfo
//	@Router       /api/rns/paths [get]
func (h *RNSHandler) Paths(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeJSON(w, http.StatusOK, []rnsnode.PathInfo{})
		return
	}
	writeJSON(w, http.StatusOK, h.svc.Paths())
}
