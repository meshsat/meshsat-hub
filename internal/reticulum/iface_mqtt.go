package reticulum

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// MQTTPublisher is the subset of bus.MessageBus needed by the MQTT interface.
type MQTTPublisher interface {
	Publish(topic string, payload []byte) error
	Subscribe(topic string, handler func(topic string, payload []byte)) error
	IsConnected() bool
}

// ReticulumMQTTTopic is the dedicated MQTT topic for Reticulum packets.
const ReticulumMQTTTopic = "meshsat/reticulum/packet"

// ReticulumRoutesTopic is the MQTT topic for publishing routing hints.
const ReticulumRoutesTopic = "meshsat/reticulum/routes"

// MQTTInterface implements Interface for MQTT transport.
// Packets are published to and subscribed from a dedicated MQTT topic.
type MQTTInterface struct {
	mu      sync.RWMutex
	mqtt    MQTTPublisher
	handler PacketHandler
}

// NewMQTTInterface creates an MQTT Reticulum transport interface.
func NewMQTTInterface(mqtt MQTTPublisher) *MQTTInterface {
	return &MQTTInterface{mqtt: mqtt}
}

// Name returns the interface type.
func (m *MQTTInterface) Name() InterfaceType {
	return IfaceMQTT
}

// Cost returns zero (MQTT is free).
func (m *MQTTInterface) Cost() float64 {
	return 0
}

// MTU returns the MQTT payload limit. MQTT itself has no practical limit,
// but we cap at the Reticulum MTU.
func (m *MQTTInterface) MTU() int {
	return MTU
}

// Send publishes a Reticulum packet to the dedicated MQTT topic.
// destID is ignored for MQTT (broadcast to all subscribers).
func (m *MQTTInterface) Send(_ context.Context, _ string, packet []byte) error {
	if m.mqtt == nil {
		return fmt.Errorf("mqtt: not configured")
	}
	return m.mqtt.Publish(ReticulumMQTTTopic, packet)
}

// IsAvailable returns true if the MQTT broker is connected.
func (m *MQTTInterface) IsAvailable() bool {
	return m.mqtt != nil && m.mqtt.IsConnected()
}

// SetHandler registers a callback for inbound Reticulum packets.
func (m *MQTTInterface) SetHandler(h PacketHandler) {
	m.mu.Lock()
	m.handler = h
	m.mu.Unlock()
}

// Start subscribes to the Reticulum MQTT topic and dispatches incoming
// packets to the registered handler.
func (m *MQTTInterface) Start() error {
	if m.mqtt == nil {
		return fmt.Errorf("mqtt: not configured")
	}
	return m.mqtt.Subscribe(ReticulumMQTTTopic, func(_ string, payload []byte) {
		m.mu.RLock()
		h := m.handler
		m.mu.RUnlock()

		if h == nil {
			slog.Warn("reticulum: mqtt packet received but no handler registered", "size", len(payload))
			return
		}
		slog.Debug("reticulum: mqtt packet received", "size", len(payload))
		h(IfaceMQTT, payload)
	})
}
