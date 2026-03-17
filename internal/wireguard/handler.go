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

// ListPeers returns all WireGuard peers.
// GET /api/wireguard/peers
func (h *APIHandler) ListPeers(w http.ResponseWriter, r *http.Request) {
	peers, err := h.client.ListPeers(r.Context())
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(peers)
}

// CreatePeer creates a new WireGuard peer.
// POST /api/wireguard/peers
func (h *APIHandler) CreatePeer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}

	peer, err := h.client.CreatePeer(r.Context(), req.Name)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(peer)
}

// GetPeerConfig returns the WireGuard client configuration.
// GET /api/wireguard/peers/{id}/config
func (h *APIHandler) GetPeerConfig(w http.ResponseWriter, r *http.Request) {
	peerID := chi.URLParam(r, "id")
	config, err := h.client.GetPeerConfig(r.Context(), peerID)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(config))
}

// DeletePeer removes a WireGuard peer.
// DELETE /api/wireguard/peers/{id}
func (h *APIHandler) DeletePeer(w http.ResponseWriter, r *http.Request) {
	peerID := chi.URLParam(r, "id")
	if err := h.client.DeletePeer(r.Context(), peerID); err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
