package reticulum

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// ReticulumTorTopicPrefix is the MQTT topic prefix for Tor-connected peers.
const ReticulumTorTopicPrefix = "meshsat/reticulum/tor/"

// TorInterface implements Interface for Tor hidden service transport.
// Reticulum packets are proxied via MQTT to peers connected through the
// Tor hidden service (.onion:1883 → Mosquitto). Field devices subscribe
// to meshsat/reticulum/tor/{destHash} to receive packets routed via Tor.
type TorInterface struct {
	mu        sync.RWMutex
	handler   PacketHandler
	available bool
	onion     string // .onion address
	mqtt      MQTTPublisher
}

// NewTorInterface creates a Tor Reticulum transport interface.
// mqtt is used to proxy Send via MQTT topics (since Tor-connected devices
// reach the Hub via the Mosquitto broker exposed on .onion:1883).
func NewTorInterface(onionAddr string, mqtt MQTTPublisher) *TorInterface {
	return &TorInterface{
		onion:     onionAddr,
		available: onionAddr != "",
		mqtt:      mqtt,
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

// Send transmits a Reticulum packet via Tor by publishing to an MQTT topic
// that Tor-connected peers subscribe to. destID is the peer's destination hash.
func (t *TorInterface) Send(_ context.Context, destID string, packet []byte) error {
	if !t.available {
		return fmt.Errorf("tor: not available")
	}
	if t.mqtt == nil {
		return fmt.Errorf("tor: no mqtt proxy configured")
	}
	topic := ReticulumTorTopicPrefix + destID
	slog.Debug("reticulum: sending packet via tor/mqtt proxy", "dest", destID, "topic", topic, "size", len(packet))
	return t.mqtt.Publish(topic, 1, false, packet)
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
