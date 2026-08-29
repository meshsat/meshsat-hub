package tak

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/meshsat/meshsat-hub/internal/bus"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/protocol"
)

// bridgeStaleSec is the CoT stale time for bridge entities (120s per MIL-STD-2525 infrastructure).
const bridgeStaleSec = 120

// Subscriber listens on Hub MQTT topics and forwards device messages to a TAK server as CoT events.
type Subscriber struct {
	mqtt           bus.MessageBus
	client         *Client
	callsignPrefix string
	cotStaleSec    int
	bridgeBirths   map[string]*protocol.BridgeBirth // bridge_id -> last birth cert
	deviceBirths   map[string]*protocol.DeviceBirth // device_id -> last birth cert
	birthMu        sync.RWMutex
}

// NewSubscriber creates a new TAK MQTT subscriber.
func NewSubscriber(mqtt bus.MessageBus, client *Client, callsignPrefix string, cotStaleSec int) *Subscriber {
	if callsignPrefix == "" {
		callsignPrefix = "MESHSAT-HUB"
	}
	if cotStaleSec <= 0 {
		cotStaleSec = 600
	}
	return &Subscriber{
		mqtt:           mqtt,
		client:         client,
		callsignPrefix: callsignPrefix,
		cotStaleSec:    cotStaleSec,
		bridgeBirths:   make(map[string]*protocol.BridgeBirth),
		deviceBirths:   make(map[string]*protocol.DeviceBirth),
	}
}

// Start subscribes to device MQTT topics and begins forwarding to TAK.
func (s *Subscriber) Start() error {
	subs := []struct {
		topic   string
		handler func(string, []byte)
	}{
		{"meshsat/+/position", s.handlePosition},
		{"meshsat/+/sos", s.handleSOS},
		{"meshsat/+/telemetry", s.handleTelemetry},
		{"meshsat/+/mo/decoded", s.handleMODecoded},
		{protocol.SubBridgeBirth, s.handleBridgeBirthCoT},
		{protocol.SubBridgeHealth, s.handleBridgeHealthCoT},
		{protocol.SubDeviceBirth, s.handleDeviceBirthCoT},
	}

	for _, sub := range subs {
		if err := s.mqtt.Subscribe(sub.topic, 1, sub.handler); err != nil {
			return fmt.Errorf("tak subscriber: %w", err)
		}
	}

	// Set up reverse path: inbound CoT → MQTT
	s.client.SetEventHandler(s.handleInboundCoT)

	slog.Info("tak: subscriber started",
		"callsign_prefix", s.callsignPrefix,
		"cot_stale_sec", s.cotStaleSec,
	)
	return nil
}

// positionMessage is the JSON format on meshsat/{device_id}/position.
type positionMessage struct {
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	Alt       float64 `json:"alt,omitempty"`
	Source    string  `json:"source,omitempty"`
	Timestamp string  `json:"timestamp,omitempty"`
}

func (s *Subscriber) handlePosition(topic string, payload []byte) {
	deviceID := hubmqtt.ExtractDeviceID(topic)
	if deviceID == "" {
		return
	}

	var pos positionMessage
	if err := json.Unmarshal(payload, &pos); err != nil {
		slog.Debug("tak: invalid position JSON", "error", err, "device", deviceID)
		return
	}

	if pos.Lat == 0 && pos.Lon == 0 {
		return // skip null-island positions
	}

	uid := fmt.Sprintf("meshsat-%s", deviceID)
	callsign := fmt.Sprintf("%s-%s", s.callsignPrefix, shortID(deviceID))
	source := pos.Source
	if source == "" {
		source = "gps"
	}

	// Enrich with device birth certificate if available.
	cotType := ""
	s.birthMu.RLock()
	if db, ok := s.deviceBirths[deviceID]; ok {
		if db.CoTCallsign != "" {
			callsign = db.CoTCallsign
		} else if db.Label != "" {
			callsign = db.Label
		}
		if db.CoTType != "" {
			cotType = db.CoTType
		} else if db.Type != "" {
			cotType = protocol.CoTTypeForDevice(db.Type)
		}
	}
	s.birthMu.RUnlock()

	ev := BuildPositionEventTyped(uid, callsign, pos.Lat, pos.Lon, pos.Alt, s.cotStaleSec, source, cotType)
	if err := s.client.Send(ev); err != nil {
		slog.Warn("tak: send position failed", "error", err, "device", deviceID)
		return
	}
	slog.Debug("tak: position forwarded", "device", deviceID, "lat", pos.Lat, "lon", pos.Lon)
}

// sosMessage is the JSON format on meshsat/{device_id}/sos.
type sosMessage struct {
	Triggered bool    `json:"triggered"`
	Lat       float64 `json:"lat,omitempty"`
	Lon       float64 `json:"lon,omitempty"`
	Timestamp string  `json:"timestamp,omitempty"`
}

func (s *Subscriber) handleSOS(topic string, payload []byte) {
	deviceID := hubmqtt.ExtractDeviceID(topic)
	if deviceID == "" {
		return
	}

	var sos sosMessage
	if err := json.Unmarshal(payload, &sos); err != nil {
		slog.Debug("tak: invalid SOS JSON", "error", err, "device", deviceID)
		return
	}

	if !sos.Triggered {
		return // SOS cancelled, don't forward
	}

	uid := fmt.Sprintf("meshsat-%s", deviceID)
	callsign := fmt.Sprintf("%s-%s", s.callsignPrefix, shortID(deviceID))

	ev := BuildSOSEvent(uid, callsign, sos.Lat, sos.Lon, s.cotStaleSec)
	if err := s.client.Send(ev); err != nil {
		slog.Warn("tak: send SOS failed", "error", err, "device", deviceID)
		return
	}
	slog.Info("tak: SOS forwarded", "device", deviceID)
}

// telemetryMessage is the JSON format on meshsat/{device_id}/telemetry.
type telemetryMessage struct {
	Battery     float64 `json:"battery,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	Humidity    float64 `json:"humidity,omitempty"`
	Pressure    float64 `json:"pressure,omitempty"`
	Lat         float64 `json:"lat,omitempty"`
	Lon         float64 `json:"lon,omitempty"`
	Timestamp   string  `json:"timestamp,omitempty"`
}

func (s *Subscriber) handleTelemetry(topic string, payload []byte) {
	deviceID := hubmqtt.ExtractDeviceID(topic)
	if deviceID == "" {
		return
	}

	var tel telemetryMessage
	if err := json.Unmarshal(payload, &tel); err != nil {
		slog.Debug("tak: invalid telemetry JSON", "error", err, "device", deviceID)
		return
	}

	uid := fmt.Sprintf("meshsat-%s", deviceID)
	callsign := fmt.Sprintf("%s-%s", s.callsignPrefix, shortID(deviceID))

	data := fmt.Sprintf("battery=%.0f%% temp=%.1fC humidity=%.0f%% pressure=%.0fhPa",
		tel.Battery, tel.Temperature, tel.Humidity, tel.Pressure)

	ev := BuildTelemetryEvent(uid, callsign, tel.Lat, tel.Lon, s.cotStaleSec, data)
	if err := s.client.Send(ev); err != nil {
		slog.Warn("tak: send telemetry failed", "error", err, "device", deviceID)
		return
	}
	slog.Debug("tak: telemetry forwarded", "device", deviceID)
}

// moDecodedMessage is the JSON format on meshsat/{device_id}/mo/decoded.
type moDecodedMessage struct {
	IMEI       string  `json:"imei"`
	Text       string  `json:"text"`
	IridiumLat float64 `json:"iridium_latitude,omitempty"`
	IridiumLon float64 `json:"iridium_longitude,omitempty"`
	IridiumCEP float64 `json:"iridium_cep,omitempty"`
	Timestamp  string  `json:"transmit_time,omitempty"`
}

func (s *Subscriber) handleMODecoded(topic string, payload []byte) {
	deviceID := hubmqtt.ExtractDeviceID(topic)
	if deviceID == "" {
		return
	}

	var mo moDecodedMessage
	if err := json.Unmarshal(payload, &mo); err != nil {
		slog.Debug("tak: invalid mo/decoded JSON", "error", err, "device", deviceID)
		return
	}

	uid := fmt.Sprintf("meshsat-%s", deviceID)
	callsign := fmt.Sprintf("%s-%s", s.callsignPrefix, shortID(deviceID))

	// If text present, send as chat
	if mo.Text != "" {
		ev := BuildChatEvent(uid, callsign, mo.Text, s.cotStaleSec)
		if err := s.client.Send(ev); err != nil {
			slog.Warn("tak: send chat failed", "error", err, "device", deviceID)
		}
	}

	// If Iridium position available, also send PLI
	if mo.IridiumLat != 0 || mo.IridiumLon != 0 {
		ev := BuildPositionEvent(uid, callsign, mo.IridiumLat, mo.IridiumLon, 0, s.cotStaleSec, "iridium_cep")
		if err := s.client.Send(ev); err != nil {
			slog.Warn("tak: send iridium position failed", "error", err, "device", deviceID)
		}
	}
}

// handleBridgeBirthCoT processes a bridge birth certificate and sends a CoT PLI event.
func (s *Subscriber) handleBridgeBirthCoT(topic string, payload []byte) {
	bridgeID := extractBridgeIDFromCoTTopic(topic)
	if bridgeID == "" {
		return
	}

	var birth protocol.BridgeBirth
	if err := json.Unmarshal(payload, &birth); err != nil {
		slog.Debug("tak: invalid bridge birth JSON", "error", err, "bridge", bridgeID)
		return
	}

	// Cache the birth certificate for health updates.
	s.birthMu.Lock()
	s.bridgeBirths[birth.BridgeID] = &birth
	s.birthMu.Unlock()

	ev := BuildBridgeEvent(birth, bridgeStaleSec)
	if err := s.client.Send(ev); err != nil {
		slog.Warn("tak: send bridge birth failed", "error", err, "bridge", bridgeID)
		return
	}
	slog.Debug("tak: bridge birth forwarded", "bridge", bridgeID, "callsign", birth.CoTCallsign)
}

// handleBridgeHealthCoT processes a bridge health update and refreshes the CoT PLI event.
func (s *Subscriber) handleBridgeHealthCoT(topic string, payload []byte) {
	bridgeID := extractBridgeIDFromCoTTopic(topic)
	if bridgeID == "" {
		return
	}

	var health protocol.BridgeHealth
	if err := json.Unmarshal(payload, &health); err != nil {
		slog.Debug("tak: invalid bridge health JSON", "error", err, "bridge", bridgeID)
		return
	}

	// Look up cached birth certificate for location and callsign.
	s.birthMu.RLock()
	birth := s.bridgeBirths[health.BridgeID]
	s.birthMu.RUnlock()

	ev := BuildBridgeHealthEvent(health, birth, bridgeStaleSec)
	if err := s.client.Send(ev); err != nil {
		slog.Warn("tak: send bridge health failed", "error", err, "bridge", bridgeID)
		return
	}
	slog.Debug("tak: bridge health forwarded", "bridge", bridgeID)
}

// handleDeviceBirthCoT processes a device birth and sends a CoT PLI event if the device has position.
func (s *Subscriber) handleDeviceBirthCoT(topic string, payload []byte) {
	bridgeID := extractBridgeIDFromCoTTopic(topic)
	deviceID := extractDeviceIDFromCoTTopic(topic)
	if bridgeID == "" || deviceID == "" {
		return
	}

	var device protocol.DeviceBirth
	if err := json.Unmarshal(payload, &device); err != nil {
		slog.Debug("tak: invalid device birth JSON", "error", err, "bridge", bridgeID, "device", deviceID)
		return
	}

	// Cache the device birth certificate for position enrichment.
	s.birthMu.Lock()
	s.deviceBirths[device.DeviceID] = &device
	s.birthMu.Unlock()

	ev := BuildDeviceBirthEvent(device, s.cotStaleSec)
	if err := s.client.Send(ev); err != nil {
		slog.Warn("tak: send device birth failed", "error", err, "bridge", bridgeID, "device", deviceID)
		return
	}
	slog.Debug("tak: device birth forwarded", "bridge", bridgeID, "device", deviceID)
}

// Bridge death is not explicitly handled. The CoT entity will go stale naturally
// when the stale time on the last birth/health event expires. This is simpler and
// avoids the need to send a CoT event with a past stale time.

// extractBridgeIDFromCoTTopic extracts bridge_id from "meshsat/bridge/{id}/...".
func extractBridgeIDFromCoTTopic(topic string) string {
	parts := strings.Split(topic, "/")
	if len(parts) < 4 || parts[0] != "meshsat" || parts[1] != "bridge" {
		return ""
	}
	return parts[2]
}

// extractDeviceIDFromCoTTopic extracts device_id from "meshsat/bridge/{bridge_id}/device/{device_id}/...".
func extractDeviceIDFromCoTTopic(topic string) string {
	parts := strings.Split(topic, "/")
	if len(parts) < 6 || parts[3] != "device" {
		return ""
	}
	return parts[4]
}

// handleInboundCoT processes CoT events received from the TAK server and publishes to MQTT.
func (s *Subscriber) handleInboundCoT(ev CotEvent) {
	// Extract a device-like ID from the CoT UID
	uid := ev.UID
	callsign := ""
	if ev.Detail != nil && ev.Detail.Contact != nil {
		callsign = ev.Detail.Contact.Callsign
	}

	text := ""
	if ev.Detail != nil && ev.Detail.Remarks != nil {
		text = ev.Detail.Remarks.Text
	}

	if text == "" && callsign != "" {
		text = fmt.Sprintf("[TAK:%s] position %.4f,%.4f", callsign, ev.Point.Lat, ev.Point.Lon)
	}

	// Publish CoT XML to broadcast topic so ALL bridges + Android devices receive it.
	// Android listens on meshsat/{deviceId}/tak/cot/in (GatewayService.kt line 987).
	// Bridge will listen on meshsat/broadcast/tak/cot/in.
	cotXML, err := MarshalCotEvent(ev)
	if err != nil {
		slog.Warn("tak: marshal inbound CoT failed", "error", err, "uid", uid)
		return
	}

	// Broadcast to all bridges/devices
	broadcastTopic := "meshsat/broadcast/tak/cot/in"
	if err := s.mqtt.Publish(broadcastTopic, 1, false, cotXML); err != nil {
		slog.Warn("tak: publish broadcast CoT failed", "error", err, "uid", uid)
	}

	// Also publish JSON summary to hub's internal topic (for dashboard/logging)
	msg := map[string]interface{}{
		"uid": uid, "type": ev.Type, "callsign": callsign,
		"lat": ev.Point.Lat, "lon": ev.Point.Lon, "text": text, "source": "tak",
	}
	s.mqtt.PublishJSON("meshsat/hub/tak/inbound", 0, false, msg) //nolint:errcheck
}

// shortID returns the last 4 characters of a device ID for callsign suffix.
func shortID(id string) string {
	if len(id) <= 4 {
		return id
	}
	return id[len(id)-4:]
}
