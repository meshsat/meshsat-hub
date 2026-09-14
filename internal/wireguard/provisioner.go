package wireguard

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
)

// deviceIDPattern validates device identifiers before use in peer names.
// Relaxed to accept various constellation IDs (not just 15-digit IMEI).
var deviceIDPattern = regexp.MustCompile(`^[a-zA-Z0-9]{5,20}$`)

// The Provisioner type and its peer map live in pool.go, beside the client pool
// they resolve through. Everything here takes a tenantID because a peer is
// created on THAT tenant's own wg-easy server (MESHSAT-1121).

// Hydrate loads the existing peer mapping for one tenant from its wg-easy.
// Call on startup, and after a tenant configures its server, to discover peers
// named with the meshsat- convention.
func (p *Provisioner) Hydrate(ctx context.Context, tenantID string) {
	c := p.pool.ForTenant(ctx, tenantID)
	if c == nil {
		return
	}
	peers, err := c.ListPeers(ctx)
	if err != nil {
		slog.Warn("wireguard: hydrate failed", "tenant", tenantID, "error", err)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, peer := range peers {
		// Convention: peer name is "meshsat-{device}"
		if len(peer.Name) > 8 && peer.Name[:8] == "meshsat-" {
			p.peers[peerKey(tenantID, peer.Name[8:])] = peer.ID
			n++
		}
	}
	slog.Info("wireguard: provisioner hydrated", "tenant", tenantID, "devices", n)
}

// OnDeviceCreated creates a WireGuard peer for a newly registered device on its
// tenant's own server. Returns the peer's VPN address, the Peer, or an error.
//
// A tenant with no wg-easy configured is NOT an error: WireGuard is optional,
// and registering a device must not fail because the tenant does not use a VPN.
func (p *Provisioner) OnDeviceCreated(ctx context.Context, tenantID, imei string) (string, *Peer, error) {
	if !deviceIDPattern.MatchString(imei) {
		return "", nil, fmt.Errorf("wireguard: invalid device ID format: %s", imei)
	}
	c := p.pool.ForTenant(ctx, tenantID)
	if c == nil {
		return "", nil, nil
	}
	peer, err := c.CreatePeer(ctx, "meshsat-"+imei)
	if err != nil {
		return "", nil, fmt.Errorf("wireguard: create peer for %s: %w", imei, err)
	}

	p.mu.Lock()
	p.peers[peerKey(tenantID, imei)] = peer.ID
	p.mu.Unlock()

	slog.Info("wireguard: peer auto-provisioned", "tenant", tenantID, "imei", imei,
		"peer_id", peer.ID, "address", peer.Address)
	return peer.Address, peer, nil
}

// OnDeviceDeleted removes the WireGuard peer for a deregistered device.
func (p *Provisioner) OnDeviceDeleted(ctx context.Context, tenantID, imei string) {
	p.mu.RLock()
	peerID, ok := p.peers[peerKey(tenantID, imei)]
	p.mu.RUnlock()
	if !ok {
		return // no peer for this device
	}
	c := p.pool.ForTenant(ctx, tenantID)
	if c == nil {
		return
	}
	if err := c.DeletePeer(ctx, peerID); err != nil {
		slog.Error("wireguard: delete peer failed", "tenant", tenantID, "imei", imei,
			"peer_id", peerID, "error", err)
		return
	}

	p.mu.Lock()
	delete(p.peers, peerKey(tenantID, imei))
	p.mu.Unlock()

	slog.Info("wireguard: peer removed", "tenant", tenantID, "imei", imei, "peer_id", peerID)
}

// GetPeerID returns the wg-easy peer ID for one tenant's device, or empty.
func (p *Provisioner) GetPeerID(tenantID, imei string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.peers[peerKey(tenantID, imei)]
}

// GetDeviceConfig returns the WireGuard client config for one tenant's device.
//
// The config carries the peer's PRIVATE KEY, so the tenant scoping here is not
// cosmetic: with the map keyed on device id alone, a device id that two tenants
// both used would hand one tenant the other's key material.
func (p *Provisioner) GetDeviceConfig(ctx context.Context, tenantID, imei string) (string, error) {
	peerID := p.GetPeerID(tenantID, imei)
	if peerID == "" {
		return "", fmt.Errorf("wireguard: no peer for device %s", imei)
	}
	c := p.pool.ForTenant(ctx, tenantID)
	if c == nil {
		return "", fmt.Errorf("wireguard: no VPN configured for this tenant")
	}
	return c.GetPeerConfig(ctx, peerID)
}
