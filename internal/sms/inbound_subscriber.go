package sms

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// InboundMQTTPayload is the JSON payload Android publishes to meshsat/{deviceId}/sms/inbound.
type InboundMQTTPayload struct {
	From      string `json:"from"`                // Sender phone number (E.164)
	To        string `json:"to,omitempty"`        // Recipient phone number
	Body      string `json:"body"`                // Message text (may be encrypted base64)
	Timestamp string `json:"timestamp,omitempty"` // ISO 8601 timestamp
}

// InboundSubscriber listens on meshsat/+/sms/inbound for SMS messages
// forwarded by Android devices via MQTT and persists them to the database.
type InboundSubscriber struct {
	mqtt     bus.MessageBus
	store    store.Store
	keyStore *hubcrypto.KeyStore
	tenantID string            // fallback when no resolver is wired
	tenants  *tenancy.Resolver // device + topic -> owning tenant
}

// SetTenants makes each message land in the tenant that owns the device, rather
// than all of them in the one this subscriber was constructed with. It
// subscribes to both topic shapes, so without this every tenant's inbound SMS
// was persisted into the default tenant (MESHSAT-975).
func (s *InboundSubscriber) SetTenants(r *tenancy.Resolver) { s.tenants = r }

// tenantFor resolves the owning tenant from the device and the topic it
// arrived on, falling back to the configured tenant only when no resolver is
// wired.
func (s *InboundSubscriber) tenantFor(ctx context.Context, topic, deviceID string) string {
	if s.tenants == nil {
		return s.tenantID
	}
	return s.tenants.ForDeviceTopic(ctx, deviceID, hubmqtt.ExtractTenantID(topic))
}

// NewInboundSubscriber creates an inbound SMS MQTT subscriber.
func NewInboundSubscriber(mqtt bus.MessageBus, s store.Store, tenantID string) *InboundSubscriber {
	return &InboundSubscriber{
		mqtt:     mqtt,
		store:    s,
		tenantID: tenantID,
	}
}

// SetKeyStore enables decryption of encrypted SMS messages from Android.
func (s *InboundSubscriber) SetKeyStore(ks *hubcrypto.KeyStore) {
	s.keyStore = ks
}

// Start subscribes to the MQTT topic for inbound SMS from Android devices.
func (s *InboundSubscriber) Start() error {
	for _, f := range hubmqtt.DualFilters("meshsat/+/sms/inbound") {
		if err := s.mqtt.Subscribe(f, 1, s.handleInbound); err != nil {
			return err
		}
	}
	return nil
}

func (s *InboundSubscriber) handleInbound(topic string, payload []byte) {
	deviceID := hubmqtt.ExtractDeviceID(topic)
	if deviceID == "" {
		slog.Warn("sms: inbound MQTT message with no device ID", "topic", topic)
		return
	}

	var msg InboundMQTTPayload
	if err := json.Unmarshal(payload, &msg); err != nil {
		slog.Warn("sms: invalid inbound MQTT JSON", "error", err, "device", deviceID)
		return
	}

	if msg.Body == "" {
		slog.Warn("sms: empty body in inbound MQTT SMS", "device", deviceID)
		return
	}

	body := msg.Body
	status := "received"

	// Attempt decryption if keystore is available.
	if s.keyStore != nil {
		if decrypted, err := s.keyStore.DecryptMessage(deviceID, []byte(msg.Body)); err == nil {
			body = string(decrypted)
			status = "decrypted"
		}
	}

	sender := msg.From
	if sender == "" {
		sender = deviceID
	}

	m := &store.Message{
		ID:         fmt.Sprintf("sms-mqtt-%d", time.Now().UnixNano()),
		DeviceIMEI: sender,
		Direction:  "mo",
		Channel:    "sms",
		Text:       body,
		Status:     status,
	}

	if s.store == nil {
		slog.Warn("sms: no store configured, skipping persist", "device", deviceID)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.store.InsertMessage(ctx, s.tenantFor(ctx, topic, deviceID), m); err != nil {
		slog.Warn("sms: failed to persist inbound MQTT SMS", "error", err, "device", deviceID)
		return
	}

	slog.Info("sms: inbound MQTT SMS persisted",
		"device", deviceID,
		"from", sender,
		"status", status,
		"text_len", len(body),
	)
}
