// Package bridge implements the Hub-side MQTT subscriber for bridge lifecycle events.
// It listens for birth/death/health messages from field bridges and auto-provisions
// bridge and device records in the Hub store.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cubeos-app/meshsat-hub/internal/bus"
	"github.com/cubeos-app/meshsat-hub/internal/protocol"
	"github.com/cubeos-app/meshsat-hub/internal/store"
)

// Subscriber listens on bridge lifecycle MQTT topics and auto-provisions
// bridge and device records in the Hub store.
type Subscriber struct {
	mqtt     bus.MessageBus
	store    store.Store
	tenantID string
}

// NewSubscriber creates a new bridge MQTT subscriber.
func NewSubscriber(mqtt bus.MessageBus, store store.Store, defaultTenantID string) *Subscriber {
	if defaultTenantID == "" {
		defaultTenantID = "default"
	}
	return &Subscriber{
		mqtt:     mqtt,
		store:    store,
		tenantID: defaultTenantID,
	}
}

// Start subscribes to all bridge lifecycle MQTT topics.
func (s *Subscriber) Start() error {
	subs := []struct {
		topic   string
		handler func(string, []byte)
	}{
		{protocol.SubBridgeBirth, s.handleBridgeBirth},
		{protocol.SubBridgeDeath, s.handleBridgeDeath},
		{protocol.SubBridgeHealth, s.handleBridgeHealth},
		{protocol.SubDeviceBirth, s.handleDeviceBirth},
		{protocol.SubDeviceDeath, s.handleDeviceDeath},
	}

	for _, sub := range subs {
		if err := s.mqtt.Subscribe(sub.topic, 1, sub.handler); err != nil {
			return fmt.Errorf("bridge subscriber: %w", err)
		}
	}

	slog.Info("bridge: subscriber started",
		"tenant_id", s.tenantID,
		"topics", len(subs),
	)
	return nil
}

// Stop is a no-op placeholder for future unsubscribe logic.
func (s *Subscriber) Stop() {
	slog.Info("bridge: subscriber stopped")
}

// handleBridgeBirth processes a bridge birth certificate.
func (s *Subscriber) handleBridgeBirth(topic string, payload []byte) {
	bridgeID := extractBridgeIDFromTopic(topic)
	if bridgeID == "" {
		slog.Debug("bridge: birth on unparseable topic", "topic", topic)
		return
	}

	var birth protocol.BridgeBirth
	if err := json.Unmarshal(payload, &birth); err != nil {
		slog.Debug("bridge: invalid birth JSON", "error", err, "bridge", bridgeID)
		return
	}

	if birth.Protocol != protocol.ProtocolVersion {
		slog.Warn("bridge: unknown protocol version in birth",
			"bridge", bridgeID,
			"protocol", birth.Protocol,
			"expected", protocol.ProtocolVersion,
		)
		return
	}

	// Marshal the full birth cert for storage.
	birthJSON, _ := json.Marshal(birth)

	// Build capabilities JSON array.
	capsJSON, _ := json.Marshal(birth.Capabilities)

	b := &store.Bridge{
		BridgeID:     birth.BridgeID,
		TenantID:     s.resolveTenantID(birth.TenantID),
		Label:        birth.BridgeID,
		Hostname:     birth.Hostname,
		Version:      birth.Version,
		Mode:         birth.Mode,
		Capabilities: string(capsJSON),
		CoTType:      birth.CoTType,
		CoTCallsign:  birth.CoTCallsign,
		Online:       true,
		LastBirth:    string(birthJSON),
	}

	if birth.Location != nil {
		b.LocationLat = birth.Location.Lat
		b.LocationLon = birth.Location.Lon
		b.LocationAlt = birth.Location.Alt
	}

	if birth.Reticulum != nil {
		b.ReticulumHash = birth.Reticulum.IdentityHash
		b.ReticulumPubkey = birth.Reticulum.PublicKey
	}

	ctx := context.Background()
	tenantID := s.resolveTenantID(birth.TenantID)

	if err := s.store.CreateOrUpdateBridge(ctx, tenantID, b); err != nil {
		slog.Error("bridge: failed to create/update bridge", "error", err, "bridge", bridgeID)
		return
	}

	if err := s.store.SetBridgeOnline(ctx, tenantID, bridgeID, true); err != nil {
		slog.Error("bridge: failed to set bridge online", "error", err, "bridge", bridgeID)
	}

	// Associate devices with IMEIs from the birth interfaces.
	for _, iface := range birth.Interfaces {
		if iface.IMEI != "" {
			if err := s.store.AssociateDeviceWithBridge(ctx, tenantID, iface.IMEI, bridgeID); err != nil {
				slog.Debug("bridge: failed to associate device",
					"error", err, "bridge", bridgeID, "imei", iface.IMEI)
			}
		}
	}

	slog.Info("bridge: registered",
		"bridge", bridgeID,
		"version", birth.Version,
		"hostname", birth.Hostname,
		"interfaces", len(birth.Interfaces),
	)
}

// handleBridgeDeath processes a bridge death notification.
func (s *Subscriber) handleBridgeDeath(topic string, payload []byte) {
	bridgeID := extractBridgeIDFromTopic(topic)
	if bridgeID == "" {
		slog.Debug("bridge: death on unparseable topic", "topic", topic)
		return
	}

	var death protocol.BridgeDeath
	if err := json.Unmarshal(payload, &death); err != nil {
		slog.Debug("bridge: invalid death JSON", "error", err, "bridge", bridgeID)
		return
	}

	if death.Protocol != protocol.ProtocolVersion {
		slog.Warn("bridge: unknown protocol version in death",
			"bridge", bridgeID,
			"protocol", death.Protocol,
			"expected", protocol.ProtocolVersion,
		)
		return
	}

	ctx := context.Background()
	tenantID := s.tenantID

	if err := s.store.SetBridgeOnline(ctx, tenantID, bridgeID, false); err != nil {
		slog.Error("bridge: failed to set bridge offline", "error", err, "bridge", bridgeID)
		return
	}

	slog.Info("bridge: offline",
		"bridge", bridgeID,
		"reason", death.Reason,
	)
}

// handleBridgeHealth processes a periodic health report.
func (s *Subscriber) handleBridgeHealth(topic string, payload []byte) {
	bridgeID := extractBridgeIDFromTopic(topic)
	if bridgeID == "" {
		return
	}

	var health protocol.BridgeHealth
	if err := json.Unmarshal(payload, &health); err != nil {
		slog.Debug("bridge: invalid health JSON", "error", err, "bridge", bridgeID)
		return
	}

	if health.Protocol != protocol.ProtocolVersion {
		return
	}

	ctx := context.Background()
	tenantID := s.tenantID

	if err := s.store.SetBridgeHealth(ctx, tenantID, bridgeID, string(payload)); err != nil {
		slog.Debug("bridge: failed to set health", "error", err, "bridge", bridgeID)
	}

	if err := s.store.TouchBridgeLastSeen(ctx, tenantID, bridgeID); err != nil {
		slog.Debug("bridge: failed to touch last_seen", "error", err, "bridge", bridgeID)
	}
}

// handleDeviceBirth processes a device coming online under a bridge.
func (s *Subscriber) handleDeviceBirth(topic string, payload []byte) {
	bridgeID := extractBridgeIDFromTopic(topic)
	deviceID := extractDeviceIDFromTopic(topic)
	if bridgeID == "" || deviceID == "" {
		slog.Debug("bridge: device birth on unparseable topic", "topic", topic)
		return
	}

	var birth protocol.DeviceBirth
	if err := json.Unmarshal(payload, &birth); err != nil {
		slog.Debug("bridge: invalid device birth JSON", "error", err, "bridge", bridgeID, "device", deviceID)
		return
	}

	if birth.Protocol != protocol.ProtocolVersion {
		slog.Warn("bridge: unknown protocol version in device birth",
			"bridge", bridgeID,
			"device", deviceID,
			"protocol", birth.Protocol,
			"expected", protocol.ProtocolVersion,
		)
		return
	}

	ctx := context.Background()
	tenantID := s.tenantID

	// Auto-provision device if it has an IMEI and doesn't exist yet.
	if birth.IMEI != "" {
		existing, err := s.store.GetDevice(ctx, tenantID, birth.IMEI)
		if err != nil || existing == nil {
			label := birth.Label
			if label == "" {
				label = birth.DeviceID
			}
			dev := &store.Device{
				IMEI:  birth.IMEI,
				Label: label,
				Type:  birth.Type,
			}
			if err := s.store.CreateDevice(ctx, tenantID, dev); err != nil {
				slog.Debug("bridge: failed to auto-provision device",
					"error", err, "bridge", bridgeID, "imei", birth.IMEI)
			} else {
				slog.Info("bridge: auto-provisioned device",
					"bridge", bridgeID,
					"imei", birth.IMEI,
					"type", birth.Type,
				)
			}
		}

		if err := s.store.AssociateDeviceWithBridge(ctx, tenantID, birth.IMEI, bridgeID); err != nil {
			slog.Debug("bridge: failed to associate device",
				"error", err, "bridge", bridgeID, "imei", birth.IMEI)
		}
	}

	slog.Info("bridge: device online",
		"bridge", bridgeID,
		"device", deviceID,
		"type", birth.Type,
		"imei", birth.IMEI,
	)
}

// handleDeviceDeath processes a device going offline.
func (s *Subscriber) handleDeviceDeath(topic string, payload []byte) {
	bridgeID := extractBridgeIDFromTopic(topic)
	deviceID := extractDeviceIDFromTopic(topic)
	if bridgeID == "" || deviceID == "" {
		return
	}

	var death protocol.DeviceDeath
	if err := json.Unmarshal(payload, &death); err != nil {
		slog.Debug("bridge: invalid device death JSON", "error", err, "bridge", bridgeID, "device", deviceID)
		return
	}

	if death.Protocol != protocol.ProtocolVersion {
		return
	}

	slog.Info("bridge: device offline",
		"bridge", bridgeID,
		"device", deviceID,
		"reason", death.Reason,
	)
}

// resolveTenantID returns the tenant ID from the birth cert if set, otherwise the default.
func (s *Subscriber) resolveTenantID(birthTenantID string) string {
	if birthTenantID != "" {
		return birthTenantID
	}
	return s.tenantID
}

// extractBridgeIDFromTopic extracts bridge_id from "meshsat/bridge/{id}/...".
func extractBridgeIDFromTopic(topic string) string {
	// meshsat/bridge/{bridge_id}/birth
	// meshsat/bridge/{bridge_id}/death
	// meshsat/bridge/{bridge_id}/health
	// meshsat/bridge/{bridge_id}/device/{device_id}/birth
	parts := strings.Split(topic, "/")
	if len(parts) < 4 || parts[0] != "meshsat" || parts[1] != "bridge" {
		return ""
	}
	return parts[2]
}

// extractDeviceIDFromTopic extracts device_id from "meshsat/bridge/{bridge_id}/device/{device_id}/...".
func extractDeviceIDFromTopic(topic string) string {
	// meshsat/bridge/{bridge_id}/device/{device_id}/birth|death
	parts := strings.Split(topic, "/")
	if len(parts) < 6 || parts[3] != "device" {
		return ""
	}
	return parts[4]
}
