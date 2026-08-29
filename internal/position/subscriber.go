// Package position handles GPS position ingestion from MQTT, webhooks, and
// the compact GPS binary codec. It stores positions to the database and
// optionally applies Douglas-Peucker track simplification on retrieval.
package position

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/deadman"
	"github.com/meshsat/meshsat-hub/internal/geo"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// positionPayload is the JSON schema published on meshsat/{device}/position.
type positionPayload struct {
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	Alt       float64 `json:"alt,omitempty"`
	Speed     float64 `json:"speed,omitempty"`
	Heading   float64 `json:"heading,omitempty"`
	Sats      int     `json:"sats,omitempty"`
	CEP       float64 `json:"cep,omitempty"`
	Source    string  `json:"source"`
	Timestamp string  `json:"timestamp"`
	// Raw is an optional base64-encoded compact GPS binary frame.
	Raw string `json:"raw,omitempty"`
}

// Subscriber listens on meshsat/+/position and stores positions to the database.
type Subscriber struct {
	bus      bus.MessageBus
	store    store.Store
	tenantID string // default tenant for MQTT-ingested positions
	deadman  *deadman.Monitor
}

// NewSubscriber creates a position subscriber.
func NewSubscriber(b bus.MessageBus, s store.Store, defaultTenantID string) *Subscriber {
	return &Subscriber{bus: b, store: s, tenantID: defaultTenantID}
}

// SetDeadman attaches a dead man's switch monitor for check-in on position updates.
func (s *Subscriber) SetDeadman(dm *deadman.Monitor) {
	s.deadman = dm
}

// Start subscribes to the position wildcard topic.
func (s *Subscriber) Start() error {
	return s.bus.Subscribe("meshsat/+/position", 1, s.handlePosition)
}

func (s *Subscriber) handlePosition(topic string, payload []byte) {
	deviceID := hubmqtt.ExtractDeviceID(topic)
	if deviceID == "" {
		slog.Warn("position: cannot extract device ID", "topic", topic)
		return
	}

	var msg positionPayload
	if err := json.Unmarshal(payload, &msg); err != nil {
		slog.Warn("position: invalid JSON", "error", err, "device", deviceID)
		return
	}

	// If a raw GPS binary frame is included, decode it for richer fields.
	// Supports hub format (0xA5, BE, ×1e7) and bridge/Android format (0x50/0x44, LE, ×1e6).
	if msg.Raw != "" {
		if raw, err := base64.StdEncoding.DecodeString(msg.Raw); err == nil {
			var gps *geo.GPSPosition
			switch {
			case geo.IsGPSFrame(raw):
				gps, _ = geo.DecodeGPS(raw)
			case len(raw) >= 16 && raw[0] == 0x50:
				gps, _ = geo.DecodeBridgeGPSFull(raw)
			case len(raw) >= 11 && raw[0] == 0x44:
				// Delta frame — resolve against last known position.
				ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
				prev, err2 := s.store.LatestPosition(ctx2, s.tenantID, deviceID)
				cancel2()
				if err2 == nil && prev != nil {
					prevGPS := &geo.GPSPosition{Lat: prev.Lat, Lon: prev.Lon, Alt: prev.Alt}
					gps, _ = geo.DecodeBridgeGPSDelta(raw, prevGPS)
				} else {
					slog.Warn("position: delta frame without prior position, skipping",
						"device", deviceID)
				}
			}
			if gps != nil {
				msg.Lat = gps.Lat
				msg.Lon = gps.Lon
				if gps.HasAlt {
					msg.Alt = gps.Alt
				}
				if gps.HasSpeed {
					msg.Speed = gps.Speed
				}
				if gps.HasHeading {
					msg.Heading = gps.Heading
				}
				if gps.HasSats {
					msg.Sats = gps.Sats
				}
				if !gps.Timestamp.IsZero() {
					msg.Timestamp = gps.Timestamp.Format(time.RFC3339)
				}
				if msg.Source == "" {
					msg.Source = "gps"
				}
			}
		}
	}

	if msg.Lat == 0 && msg.Lon == 0 {
		return // skip null-island positions
	}

	if msg.Source == "" {
		msg.Source = "gps"
	}

	pos := &store.Position{
		ID:         fmt.Sprintf("pos-%d", time.Now().UnixNano()),
		DeviceIMEI: deviceID,
		Lat:        msg.Lat,
		Lon:        msg.Lon,
		Alt:        msg.Alt,
		Speed:      msg.Speed,
		Heading:    msg.Heading,
		Sats:       msg.Sats,
		Source:     msg.Source,
		CEP:        msg.CEP,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.store.InsertPosition(ctx, s.tenantID, pos); err != nil {
		slog.Warn("position: store failed", "error", err, "device", deviceID)
		return
	}

	// Touch device last_seen and dead man's switch check-in.
	_ = s.store.TouchDeviceLastSeen(ctx, s.tenantID, deviceID)
	if s.deadman != nil {
		s.deadman.CheckIn(deviceID)
	}

	slog.Debug("position: stored",
		"device", deviceID,
		"lat", msg.Lat,
		"lon", msg.Lon,
		"source", msg.Source,
	)
}
