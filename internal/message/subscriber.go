// Package message provides MQTT subscribers that persist MO/MT messages to the database.
package message

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// moDecodedPayload matches the JSON published to meshsat/+/mo/decoded.
type moDecodedPayload struct {
	IMEI        string  `json:"imei"`
	DeviceGUID  string  `json:"device_guid"`
	MOMSN       int     `json:"momsn"`
	Channel     string  `json:"channel"`
	Text        string  `json:"text"`
	Raw         string  `json:"raw"`
	Compressed  bool    `json:"compressed"`
	Compression string  `json:"compression"`
	Encrypted   bool    `json:"encrypted"`
	Lat         float64 `json:"iridium_latitude"`
	Lon         float64 `json:"iridium_longitude"`
}

// Subscriber persists MO decoded messages from MQTT to the database.
type Subscriber struct {
	bus      bus.MessageBus
	store    store.Store
	tenantID string
}

// NewSubscriber creates a message persistence subscriber.
func NewSubscriber(b bus.MessageBus, s store.Store, tenantID string) *Subscriber {
	return &Subscriber{bus: b, store: s, tenantID: tenantID}
}

// Start subscribes to mo/decoded and persists messages.
func (s *Subscriber) Start() error {
	return s.bus.Subscribe("meshsat/+/mo/decoded", 1, s.handleMODecoded)
}

func (s *Subscriber) handleMODecoded(topic string, payload []byte) {
	deviceID := hubmqtt.ExtractDeviceID(topic)
	if deviceID == "" {
		return
	}

	var msg moDecodedPayload
	if err := json.Unmarshal(payload, &msg); err != nil {
		slog.Warn("message: invalid mo/decoded JSON", "error", err, "device", deviceID)
		return
	}

	// Use IMEI if available, fall back to device GUID.
	imei := msg.IMEI
	if imei == "" {
		imei = msg.DeviceGUID
	}
	if imei == "" {
		imei = deviceID
	}

	m := &store.Message{
		ID:         fmt.Sprintf("mo-%d", time.Now().UnixNano()),
		DeviceIMEI: imei,
		Direction:  "mo",
		Channel:    msg.Channel,
		MOMSN:      msg.MOMSN,
		Text:       msg.Text,
		RawHex:     msg.Raw,
		Compressed: msg.Compressed,
		Status:     "received",
		Lat:        msg.Lat,
		Lon:        msg.Lon,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.store.InsertMessage(ctx, s.tenantID, m); err != nil {
		slog.Warn("message: persist failed", "error", err, "device", imei)
		return
	}

	slog.Debug("message: persisted MO", "device", imei, "channel", msg.Channel, "text_len", len(msg.Text))
}
