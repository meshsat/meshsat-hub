package reticulum

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// TorInterface implements Interface for Tor hidden service transport.
// Reticulum packets are carried over a TCP connection to the Hub's .onion address.
// Currently a placeholder — full implementation requires a TCP listener on
// the Tor hidden service that speaks Reticulum framing.
type TorInterface struct {
	mu        sync.RWMutex
	handler   PacketHandler
	available bool
	onion     string // .onion address
}

// NewTorInterface creates a Tor Reticulum transport interface.
func NewTorInterface(onionAddr string) *TorInterface {
	return &TorInterface{
		onion:     onionAddr,
		available: onionAddr != "",
	}
}

// Name returns the interface type.
func (t *TorInterface) Name() InterfaceType {
	return IfaceTor
}

// Cost returns zero (Tor is free).
func (t *TorInterface) Cost() float64 {
	return 0
}

// MTU returns the Reticulum MTU (Tor has no practical payload limit).
func (t *TorInterface) MTU() int {
	return MTU
}

// Send transmits a Reticulum packet via Tor. destID is the peer's .onion address.
// TODO: Implement TCP connection to peer's .onion Reticulum port.
func (t *TorInterface) Send(_ context.Context, destID string, packet []byte) error {
	if !t.available {
		return fmt.Errorf("tor: not available")
	}
	slog.Debug("reticulum: tor send (not yet implemented)", "dest", destID, "size", len(packet))
	return fmt.Errorf("tor: send not yet implemented")
}

// IsAvailable returns true if the Tor hidden service is running.
func (t *TorInterface) IsAvailable() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.available
}

// SetHandler registers a callback for inbound Reticulum packets from Tor.
func (t *TorInterface) SetHandler(h PacketHandler) {
	t.mu.Lock()
	t.handler = h
	t.mu.Unlock()
}

// OnReceive dispatches an inbound packet from a Tor TCP connection.
func (t *TorInterface) OnReceive(raw []byte) {
	t.mu.RLock()
	h := t.handler
	t.mu.RUnlock()

	if h == nil {
		return
	}
	slog.Debug("reticulum: tor packet received", "size", len(raw))
	h(IfaceTor, raw)
}
