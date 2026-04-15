package aprsis

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/bus"
	hubmqtt "github.com/cubeos-app/meshsat-hub/internal/mqtt"
)

// Subscriber listens on Hub MQTT position topics and injects satellite-originated
// positions into APRS-IS. Also receives APRS-IS messages addressed to MeshSat
// devices and forwards them to MQTT.
type Subscriber struct {
	mqtt   bus.MessageBus
	client *Client

	// Rate limiting: max 1 position per device per coalesceSec
	coalesceSec int
	lastSent    map[string]time.Time
	mu          sync.Mutex
}

// NewSubscriber creates a new APRS-IS MQTT subscriber.
func NewSubscriber(mqtt bus.MessageBus, client *Client, coalesceSec int) *Subscriber {
	if coalesceSec <= 0 {
		coalesceSec = 60
	}
	return &Subscriber{
		mqtt:        mqtt,
		client:      client,
		coalesceSec: coalesceSec,
		lastSent:    make(map[string]time.Time),
	}
}

// Start subscribes to position MQTT topics and sets up the APRS-IS inbound handler.
func (s *Subscriber) Start() error {
	if err := s.mqtt.Subscribe("meshsat/+/position", 1, s.handlePosition); err != nil {
		return fmt.Errorf("aprsis subscriber: %w", err)
	}
	if err := s.mqtt.Subscribe("meshsat/+/mo/decoded", 1, s.handleMODecoded); err != nil {
		return fmt.Errorf("aprsis subscriber: %w", err)
	}

	// Inbound: APRS-IS messages → MQTT
	s.client.SetPacketHandler(s.handleInboundPacket)

	slog.Info("aprsis: subscriber started", "coalesce_sec", s.coalesceSec)
	return nil
}

// positionMsg matches the JSON on meshsat/{device_id}/position.
type positionMsg struct {
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	Source    string  `json:"source,omitempty"`
	Timestamp string  `json:"timestamp,omitempty"`
}

func (s *Subscriber) handlePosition(topic string, payload []byte) {
	deviceID := hubmqtt.ExtractDeviceID(topic)
	if deviceID == "" {
		return
	}

	var pos positionMsg
	if err := json.Unmarshal(payload, &pos); err != nil {
		return
	}

	// Only inject satellite-originated positions (not mesh/SMS)
	if pos.Source != "iridium" && pos.Source != "iridium_cep" && pos.Source != "globalstar" {
		return
	}

	if pos.Lat == 0 && pos.Lon == 0 {
		return
	}

	// Rate limit per device
	if !s.shouldSend(deviceID) {
		return
	}

	comment := fmt.Sprintf("MeshSat via %s", pos.Source)
	packet := FormatPosition(s.client.callsign, s.client.ssid, pos.Lat, pos.Lon, comment)

	if err := s.client.Send(packet); err != nil {
		slog.Warn("aprsis: send position failed", "error", err, "device", deviceID)
		return
	}
	slog.Debug("aprsis: position injected", "device", deviceID, "lat", pos.Lat, "lon", pos.Lon)
}

// moDecodedMsg matches meshsat/{device_id}/mo/decoded.
type moDecodedMsg struct {
	IMEI       string  `json:"imei"`
	Text       string  `json:"text"`
	IridiumLat float64 `json:"iridium_latitude,omitempty"`
	IridiumLon float64 `json:"iridium_longitude,omitempty"`
}

func (s *Subscriber) handleMODecoded(topic string, payload []byte) {
	deviceID := hubmqtt.ExtractDeviceID(topic)
	if deviceID == "" {
		return
	}

	var mo moDecodedMsg
	if err := json.Unmarshal(payload, &mo); err != nil {
		return
	}

	// Only inject if we have Iridium coordinates
	if mo.IridiumLat == 0 && mo.IridiumLon == 0 {
		return
	}

	if !s.shouldSend(deviceID) {
		return
	}

	comment := "MeshSat via Iridium SBD"
	if mo.Text != "" {
		// Truncate text for APRS comment (max ~40 chars practical)
		text := mo.Text
		if len(text) > 40 {
			text = text[:37] + "..."
		}
		comment += " " + text
	}

	packet := FormatPosition(s.client.callsign, s.client.ssid, mo.IridiumLat, mo.IridiumLon, comment)

	if err := s.client.Send(packet); err != nil {
		slog.Warn("aprsis: send MO position failed", "error", err, "device", deviceID)
		return
	}
	slog.Debug("aprsis: MO position injected", "device", deviceID)
}

// handleInboundPacket processes APRS-IS packets addressed to MeshSat devices.
func (s *Subscriber) handleInboundPacket(line string) {
	// APRS message format: SRC>DST,PATH::ADDRESSEE :message{id
	// We're looking for messages addressed to our callsign
	myCall := strings.ToUpper(s.client.callsign)

	colonIdx := strings.Index(line, ":")
	if colonIdx < 0 || colonIdx+1 >= len(line) {
		return
	}

	info := line[colonIdx+1:]
	if len(info) < 2 || info[0] != ':' {
		return // not a message packet
	}

	// Extract addressee (9 chars padded with spaces)
	if len(info) < 11 {
		return
	}
	addressee := strings.TrimSpace(info[1:10])

	if !strings.HasPrefix(strings.ToUpper(addressee), myCall) {
		return // not for us
	}

	// Extract message text
	if len(info) < 12 || info[10] != ':' {
		return
	}
	msgText := info[11:]

	// Strip message ID if present
	if idx := strings.LastIndex(msgText, "{"); idx >= 0 {
		msgText = msgText[:idx]
	}

	// Extract source callsign
	srcEnd := strings.Index(line, ">")
	if srcEnd < 0 {
		return
	}
	srcCall := line[:srcEnd]

	slog.Info("aprsis: inbound message", "from", srcCall, "to", addressee, "text", msgText)

	// Publish to MQTT — use the addressee as a device hint
	msg := map[string]interface{}{
		"source":   "aprs-is",
		"from":     srcCall,
		"to":       addressee,
		"text":     msgText,
		"received": time.Now().UTC().Format(time.RFC3339),
	}

	topic := "meshsat/hub/aprsis/inbound"
	if err := s.mqtt.PublishJSON(topic, 1, false, msg); err != nil {
		slog.Warn("aprsis: publish inbound failed", "error", err)
	}
}

// shouldSend returns true if enough time has passed since the last send for this device.
func (s *Subscriber) shouldSend(deviceID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	if last, ok := s.lastSent[deviceID]; ok {
		if now.Sub(last) < time.Duration(s.coalesceSec)*time.Second {
			return false
		}
	}
	s.lastSent[deviceID] = now
	return true
}
