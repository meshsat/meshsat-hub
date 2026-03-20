package reticulum

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// WireGuardInterface implements Interface for WireGuard tunnel transport.
// Reticulum packets are carried over UDP through the WireGuard tunnel.
// Currently a placeholder — full implementation requires a UDP listener
// on the WireGuard interface that speaks Reticulum framing.
type WireGuardInterface struct {
	mu        sync.RWMutex
	handler   PacketHandler
	available bool
}

// NewWireGuardInterface creates a WireGuard Reticulum transport interface.
func NewWireGuardInterface(available bool) *WireGuardInterface {
	return &WireGuardInterface{available: available}
}

// Name returns the interface type.
func (w *WireGuardInterface) Name() InterfaceType {
	return IfaceWireGuard
}

// Cost returns zero (WireGuard is free).
func (w *WireGuardInterface) Cost() float64 {
	return 0
}

// MTU returns the Reticulum MTU (WireGuard has ~1400 byte MTU, well above Reticulum's 500).
func (w *WireGuardInterface) MTU() int {
	return MTU
}

// Send transmits a Reticulum packet via WireGuard. destID is the peer's WireGuard public key or IP.
// TODO: Implement UDP send to peer's WireGuard endpoint.
func (w *WireGuardInterface) Send(_ context.Context, destID string, packet []byte) error {
	if !w.available {
		return fmt.Errorf("wireguard: not available")
	}
	slog.Debug("reticulum: wireguard send (not yet implemented)", "dest", destID, "size", len(packet))
	return fmt.Errorf("wireguard: send not yet implemented")
}

// IsAvailable returns true if WireGuard is configured and running.
func (w *WireGuardInterface) IsAvailable() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.available
}

// SetAvailable updates the availability status.
func (w *WireGuardInterface) SetAvailable(avail bool) {
	w.mu.Lock()
	w.available = avail
	w.mu.Unlock()
}

// SetHandler registers a callback for inbound Reticulum packets from WireGuard.
func (w *WireGuardInterface) SetHandler(h PacketHandler) {
	w.mu.Lock()
	w.handler = h
	w.mu.Unlock()
}

// OnReceive dispatches an inbound packet from a WireGuard tunnel.
func (w *WireGuardInterface) OnReceive(raw []byte) {
	w.mu.RLock()
	h := w.handler
	w.mu.RUnlock()

	if h == nil {
		return
	}
	slog.Debug("reticulum: wireguard packet received", "size", len(raw))
	h(IfaceWireGuard, raw)
}
