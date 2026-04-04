package wireguard

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// APIHandler provides REST endpoints for WireGuard peer management.
type APIHandler struct {
	client *Client
}

// NewAPIHandler creates a new WireGuard API handler.
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
	peers, err := h.client.ListPeers(r.Context())
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

	peer, err := h.client.CreatePeer(r.Context(), req.Name)
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
	config, err := h.client.GetPeerConfig(r.Context(), peerID)
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
	if err := h.client.DeletePeer(r.Context(), peerID); err != nil {
		wgWriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
