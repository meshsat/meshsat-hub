package wireguard

import (
	"encoding/json"
	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// APIHandler provides REST endpoints for WireGuard peer management.
type APIHandler struct {
	client *Client
	pool   *ClientPool
}

// NewAPIHandlerPool builds a handler that acts on the CALLING tenant's own
// wg-easy. Peer configs carry private keys, so this is the difference between a
// customer seeing their own VPN and seeing everybody's (MESHSAT-1121).
func NewAPIHandlerPool(pool *ClientPool) *APIHandler {
	return &APIHandler{pool: pool}
}

// clientFor resolves the caller's wg-easy, or writes 503 and returns nil.
func (h *APIHandler) clientFor(w http.ResponseWriter, r *http.Request) *Client {
	if h.pool == nil {
		return h.client
	}
	c := h.pool.ForTenant(r.Context(), hubauth.TenantIDFromContext(r.Context()))
	if c == nil {
		wgWriteError(w, http.StatusServiceUnavailable,
			"no WireGuard server configured for this tenant (Integrations page)")
		return nil
	}
	return c
}

func NewAPIHandler(client *Client) *APIHandler {
	return &APIHandler{client: client}
}

// wgWriteJSON writes a JSON response with the given status code.
func wgWriteJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// wgWriteError writes a JSON error response (properly escaped, no string concatenation).
func wgWriteError(w http.ResponseWriter, status int, msg string) {
	wgWriteJSON(w, status, map[string]string{"error": msg})
}

// ListPeers returns all WireGuard peers.
// @Summary List WireGuard peers
// @Tags wireguard
// @Produce json
// @Success 200 {array} object
// @Failure 502 {object} map[string]string
// @Router /api/wireguard/peers [get]
func (h *APIHandler) ListPeers(w http.ResponseWriter, r *http.Request) {
	c := h.clientFor(w, r)
	if c == nil {
		return
	}
	peers, err := c.ListPeers(r.Context())
	if err != nil {
		wgWriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	wgWriteJSON(w, http.StatusOK, peers)
}

// CreatePeer creates a new WireGuard peer.
// @Summary Create WireGuard peer
// @Tags wireguard
// @Accept json
// @Produce json
// @Param body body object true "Peer name" example({"name": "field-device-1"})
// @Success 201 {object} object
// @Failure 400 {object} map[string]string
// @Failure 502 {object} map[string]string
// @Router /api/wireguard/peers [post]
func (h *APIHandler) CreatePeer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		wgWriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name == "" {
		wgWriteError(w, http.StatusBadRequest, "name is required")
		return
	}

	c := h.clientFor(w, r)
	if c == nil {
		return
	}
	peer, err := c.CreatePeer(r.Context(), req.Name)
	if err != nil {
		wgWriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	wgWriteJSON(w, http.StatusCreated, peer)
}

// GetPeerConfig returns the WireGuard client configuration.
// @Summary Get WireGuard peer config
// @Tags wireguard
// @Produce text/plain
// @Param id path string true "Peer ID"
// @Success 200 {string} string
// @Failure 502 {object} map[string]string
// @Router /api/wireguard/peers/{id}/config [get]
func (h *APIHandler) GetPeerConfig(w http.ResponseWriter, r *http.Request) {
	peerID := chi.URLParam(r, "id")
	c := h.clientFor(w, r)
	if c == nil {
		return
	}
	config, err := c.GetPeerConfig(r.Context(), peerID)
	if err != nil {
		wgWriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(config))
}

// DeletePeer removes a WireGuard peer.
// @Summary Delete WireGuard peer
// @Tags wireguard
// @Param id path string true "Peer ID"
// @Success 204
// @Failure 502 {object} map[string]string
// @Router /api/wireguard/peers/{id} [delete]
func (h *APIHandler) DeletePeer(w http.ResponseWriter, r *http.Request) {
	peerID := chi.URLParam(r, "id")
	c := h.clientFor(w, r)
	if c == nil {
		return
	}
	if err := c.DeletePeer(r.Context(), peerID); err != nil {
		wgWriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
