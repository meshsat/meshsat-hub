package wireguard

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
)

// deviceIDPattern validates device identifiers before use in peer names.
// Relaxed to accept Astrocast and other constellation IDs (not just 15-digit IMEI).
var deviceIDPattern = regexp.MustCompile(`^[a-zA-Z0-9]{5,20}$`)

// Provisioner auto-creates WireGuard peers when devices are registered
// and removes them when devices are deleted.
type Provisioner struct {
	client *Client
	mu     sync.RWMutex
	peers  map[string]string // device IMEI → wg-easy peer ID
}

// NewProvisioner creates a WireGuard auto-provisioner.
func NewProvisioner(client *Client) *Provisioner {
	return &Provisioner{
		client: client,
		peers:  make(map[string]string),
	}
}

// Hydrate loads the existing peer mapping from wg-easy.
// Call on startup to discover peers named with IMEI convention.
func (p *Provisioner) Hydrate(ctx context.Context) {
	peers, err := p.client.ListPeers(ctx)
	if err != nil {
		slog.Warn("wireguard: hydrate failed", "error", err)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, peer := range peers {
		// Convention: peer name is "meshsat-{imei}"
		if len(peer.Name) > 8 && peer.Name[:8] == "meshsat-" {
			imei := peer.Name[8:]
			p.peers[imei] = peer.ID
		}
	}
	slog.Info("wireguard: provisioner hydrated", "devices", len(p.peers))
}

// OnDeviceCreated creates a WireGuard peer for a newly registered device.
// Returns the peer's VPN address (e.g., "10.8.0.5/32"), the Peer object, or error.
func (p *Provisioner) OnDeviceCreated(ctx context.Context, imei string) (string, *Peer, error) {
	if !deviceIDPattern.MatchString(imei) {
		return "", nil, fmt.Errorf("wireguard: invalid device ID format: %s", imei)
	}
	peerName := "meshsat-" + imei
	peer, err := p.client.CreatePeer(ctx, peerName)
	if err != nil {
		return "", nil, fmt.Errorf("wireguard: create peer for %s: %w", imei, err)
	}

	p.mu.Lock()
	p.peers[imei] = peer.ID
	p.mu.Unlock()

	slog.Info("wireguard: peer auto-provisioned", "imei", imei, "peer_id", peer.ID, "address", peer.Address)
	return peer.Address, peer, nil
}

// OnDeviceDeleted removes the WireGuard peer for a deregistered device.
func (p *Provisioner) OnDeviceDeleted(ctx context.Context, imei string) {
	p.mu.RLock()
	peerID, ok := p.peers[imei]
	p.mu.RUnlock()

	if !ok {
		return // no peer for this device
	}

	if err := p.client.DeletePeer(ctx, peerID); err != nil {
		slog.Error("wireguard: delete peer failed", "imei", imei, "peer_id", peerID, "error", err)
		return
	}

	p.mu.Lock()
	delete(p.peers, imei)
	p.mu.Unlock()

	slog.Info("wireguard: peer removed", "imei", imei, "peer_id", peerID)
}

// GetPeerID returns the wg-easy peer ID for a device, or empty if not provisioned.
func (p *Provisioner) GetPeerID(imei string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.peers[imei]
}

// GetDeviceConfig returns the WireGuard client config for a device.
func (p *Provisioner) GetDeviceConfig(ctx context.Context, imei string) (string, error) {
	peerID := p.GetPeerID(imei)
	if peerID == "" {
		return "", fmt.Errorf("wireguard: no peer for device %s", imei)
	}
	return p.client.GetPeerConfig(ctx, peerID)
}
