package tak

import (
	"encoding/json"
	"fmt"
	"log/slog"

	hubmqtt "github.com/cubeos-app/meshsat-hub/internal/mqtt"
)

// Subscriber listens on Hub MQTT topics and forwards device messages to a TAK server as CoT events.
type Subscriber struct {
	mqtt           *hubmqtt.Client
	client         *Client
	callsignPrefix string
	cotStaleSec    int
}

// NewSubscriber creates a new TAK MQTT subscriber.
func NewSubscriber(mqtt *hubmqtt.Client, client *Client, callsignPrefix string, cotStaleSec int) *Subscriber {
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

	ev := BuildPositionEvent(uid, callsign, pos.Lat, pos.Lon, pos.Alt, s.cotStaleSec, source)
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

	// Publish to a generic TAK inbound topic
	msg := map[string]interface{}{
		"uid":      uid,
		"type":     ev.Type,
		"callsign": callsign,
		"lat":      ev.Point.Lat,
		"lon":      ev.Point.Lon,
		"text":     text,
		"source":   "tak",
	}

	topic := "meshsat/hub/tak/inbound"
	if err := s.mqtt.PublishJSON(topic, 1, false, msg); err != nil {
		slog.Warn("tak: publish inbound event failed", "error", err, "uid", uid)
	}
}

// shortID returns the last 4 characters of a device ID for callsign suffix.
func shortID(id string) string {
	if len(id) <= 4 {
		return id
	}
	return id[len(id)-4:]
}
