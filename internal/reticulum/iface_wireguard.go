package reticulum

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// ReticulumWGTopicPrefix is the MQTT topic prefix for WireGuard-connected peers.
const ReticulumWGTopicPrefix = "meshsat/reticulum/wg/"

// WireGuardInterface implements Interface for WireGuard tunnel transport.
// Reticulum packets are proxied via MQTT to peers connected through the
// WireGuard VPN tunnel. Field devices with VPN access connect to the
// MQTT broker directly and subscribe to meshsat/reticulum/wg/{destHash}.
type WireGuardInterface struct {
	mu        sync.RWMutex
	handler   PacketHandler
	available bool
	mqtt      MQTTPublisher
}

// NewWireGuardInterface creates a WireGuard Reticulum transport interface.
// mqtt is used to proxy Send via MQTT topics (since WireGuard-connected
// devices reach the MQTT broker directly through the tunnel).
func NewWireGuardInterface(available bool, mqtt MQTTPublisher) *WireGuardInterface {
	return &WireGuardInterface{available: available, mqtt: mqtt}
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

// Send transmits a Reticulum packet via WireGuard by publishing to an MQTT
// topic that VPN-connected peers subscribe to. destID is the peer's destination hash.
func (w *WireGuardInterface) Send(_ context.Context, destID string, packet []byte) error {
	if !w.available {
		return fmt.Errorf("wireguard: not available")
	}
	if w.mqtt == nil {
		return fmt.Errorf("wireguard: no mqtt proxy configured")
	}
	topic := ReticulumWGTopicPrefix + destID
	slog.Debug("reticulum: sending packet via wireguard/mqtt proxy", "dest", destID, "topic", topic, "size", len(packet))
	return w.mqtt.Publish(topic, 1, false, packet)
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
