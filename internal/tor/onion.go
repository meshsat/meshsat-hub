// Package tor provides Tor hidden service (.onion) address discovery for MeshSat Hub.
// The .onion address is read from the Tor data volume and cached on startup.
package tor

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

// OnionInfo holds the Hub's Tor .onion addresses.
type OnionInfo struct {
	HTTPAddress string `json:"http_address,omitempty"` // e.g., "abc123...xyz.onion"
	MQTTAddress string `json:"mqtt_address,omitempty"` // same .onion, different port
	Available   bool   `json:"available"`
}

// Service reads and caches the Tor .onion address.
type Service struct {
	info OnionInfo
}

// NewService creates a Tor service that reads the .onion hostname.
// hostnamePath is typically "/var/lib/tor/hidden_service/hostname".
// If the file doesn't exist (Tor not running), the service reports unavailable.
func NewService(hostnamePath string) *Service {
	s := &Service{}

	data, err := os.ReadFile(hostnamePath)
	if err != nil {
		slog.Info("tor: .onion hostname not available (Tor may not be running)", "path", hostnamePath, "error", err)
		return s
	}

	hostname := strings.TrimSpace(string(data))
	if hostname == "" {
		slog.Warn("tor: .onion hostname file is empty", "path", hostnamePath)
		return s
	}

	s.info = OnionInfo{
		HTTPAddress: hostname,
		MQTTAddress: hostname, // same .onion, MQTT on port 1883
		Available:   true,
	}
	slog.Info("tor: .onion address loaded", "address", hostname)
	return s
}

// Info returns the cached .onion address info.
func (s *Service) Info() OnionInfo {
	return s.info
}

// APIHandler provides REST endpoints for Tor .onion discovery.
type APIHandler struct {
	service *Service
}

// NewAPIHandler creates a Tor API handler.
func NewAPIHandler(svc *Service) *APIHandler {
	return &APIHandler{service: svc}
}

// GetOnion returns the Hub's Tor .onion addresses.
//
//	@Summary      Get Tor .onion address
//	@Description  Returns the Hub's Tor hidden service .onion addresses for HTTP and MQTT
//	@Tags         tor
//	@Produce      json
//	@Success      200  {object}  OnionInfo
//	@Router       /api/tor/onion [get]
func (h *APIHandler) GetOnion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.service.Info())
}
