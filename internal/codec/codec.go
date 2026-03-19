// Package codec provides a pluggable decoder framework for structured sensor
// payloads. Decoders are registered by name and selected per-device based on
// the device's payload_format configuration.
package codec

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/cubeos-app/meshsat-hub/internal/geo"
)

// DecodedPayload is the output of a decoder — structured key-value telemetry.
type DecodedPayload struct {
	Format string                 `json:"format"` // decoder name that produced this
	Fields map[string]interface{} `json:"fields"` // decoded sensor values
}

// Decoder decodes raw payload bytes into structured telemetry fields.
type Decoder interface {
	// Name returns the decoder's identifier (e.g., "gps", "json", "zigbee").
	Name() string

	// Decode attempts to decode the payload. Returns nil if the payload
	// is not recognized by this decoder.
	Decode(payload []byte) (*DecodedPayload, error)
}

// Registry manages available decoders and provides format selection.
type Registry struct {
	mu       sync.RWMutex
	decoders map[string]Decoder
	order    []string // registration order for auto-detect
}

// NewRegistry creates a new codec registry with built-in decoders.
func NewRegistry() *Registry {
	r := &Registry{
		decoders: make(map[string]Decoder),
	}
	// Register built-in decoders (order matters for auto-detect).
	r.Register(&BridgeGPSFullDecoder{})  // 0x50 — bridge/Android full position
	r.Register(&BridgeGPSDeltaDecoder{}) // 0x44 — bridge/Android delta position
	r.Register(&GPSDecoder{})            // 0xA5 — hub GPS format (internal/geo)
	r.Register(&CannedDecoder{})         // 0xCA — canned military brevity codebook
	r.Register(&JSONDecoder{})
	r.Register(&ZigBeeDecoder{})
	r.Register(&RawDecoder{})
	return r
}

// Register adds a decoder to the registry.
func (r *Registry) Register(d Decoder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decoders[d.Name()] = d
	r.order = append(r.order, d.Name())
	slog.Debug("codec: registered decoder", "name", d.Name())
}

// Decode decodes a payload using the specified format.
// If format is "auto" or empty, tries each registered decoder in order.
func (r *Registry) Decode(format string, payload []byte) (*DecodedPayload, error) {
	if format == "" || format == "auto" {
		return r.autoDetect(payload)
	}

	r.mu.RLock()
	d, ok := r.decoders[format]
	r.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("codec: unknown format %q", format)
	}

	return d.Decode(payload)
}

func (r *Registry) autoDetect(payload []byte) (*DecodedPayload, error) {
	r.mu.RLock()
	order := r.order
	r.mu.RUnlock()

	for _, name := range order {
		r.mu.RLock()
		d := r.decoders[name]
		r.mu.RUnlock()

		result, err := d.Decode(payload)
		if err == nil && result != nil {
			return result, nil
		}
	}

	return &DecodedPayload{Format: "raw", Fields: map[string]interface{}{"raw_hex": fmt.Sprintf("%x", payload)}}, nil
}

// List returns metadata about all registered decoders.
func (r *Registry) List() []CodecInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	infos := make([]CodecInfo, 0, len(r.order))
	for _, name := range r.order {
		infos = append(infos, CodecInfo{Name: name})
	}
	return infos
}

// CodecInfo holds metadata about a registered decoder.
type CodecInfo struct {
	Name string `json:"name"`
}

// --- Built-in decoders ---

// GPSDecoder decodes the compact GPS binary frame from internal/geo (0xA5, BE, ×1e7).
type GPSDecoder struct{}

func (GPSDecoder) Name() string { return "gps" }

func (GPSDecoder) Decode(payload []byte) (*DecodedPayload, error) {
	if !geo.IsGPSFrame(payload) {
		return nil, fmt.Errorf("not a GPS frame")
	}

	gps, err := geo.DecodeGPS(payload)
	if err != nil {
		return nil, err
	}

	fields := map[string]interface{}{
		"lat": gps.Lat,
		"lon": gps.Lon,
	}
	if gps.HasAlt {
		fields["alt"] = gps.Alt
	}
	if gps.HasSpeed {
		fields["speed"] = gps.Speed
	}
	if gps.HasHeading {
		fields["heading"] = gps.Heading
	}
	if gps.HasSats {
		fields["sats"] = gps.Sats
	}
	if !gps.Timestamp.IsZero() {
		fields["timestamp"] = gps.Timestamp.UTC().Format("2006-01-02T15:04:05Z")
	}

	return &DecodedPayload{Format: "gps", Fields: fields}, nil
}

// JSONDecoder passes through JSON payloads as structured telemetry.
type JSONDecoder struct{}

func (JSONDecoder) Name() string { return "json" }

func (JSONDecoder) Decode(payload []byte) (*DecodedPayload, error) {
	if len(payload) < 2 || payload[0] != '{' {
		return nil, fmt.Errorf("not JSON")
	}

	var fields map[string]interface{}
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	return &DecodedPayload{Format: "json", Fields: fields}, nil
}

// ZigBeeDecoder decodes ZigBee Cluster Library (ZCL) sensor frames.
// Supports: temperature (cluster 0x0402), humidity (0x0405), pressure (0x0403).
type ZigBeeDecoder struct{}

func (ZigBeeDecoder) Name() string { return "zigbee" }

func (ZigBeeDecoder) Decode(payload []byte) (*DecodedPayload, error) {
	// ZCL frame: min 5 bytes (frame control + seq + cluster ID + attr)
	if len(payload) < 5 {
		return nil, fmt.Errorf("too short for ZCL")
	}

	// Simple ZCL parse: look for known cluster IDs.
	// Cluster ID at bytes [2:4] (little-endian).
	clusterID := uint16(payload[2]) | uint16(payload[3])<<8

	fields := make(map[string]interface{})

	switch clusterID {
	case 0x0402: // Temperature Measurement
		if len(payload) >= 7 {
			raw := int16(payload[5]) | int16(payload[6])<<8
			fields["temperature_c"] = float64(raw) / 100.0
		}
	case 0x0405: // Relative Humidity
		if len(payload) >= 7 {
			raw := uint16(payload[5]) | uint16(payload[6])<<8
			fields["humidity_pct"] = float64(raw) / 100.0
		}
	case 0x0403: // Pressure Measurement
		if len(payload) >= 7 {
			raw := uint16(payload[5]) | uint16(payload[6])<<8
			fields["pressure_hpa"] = float64(raw) / 10.0
		}
	default:
		return nil, fmt.Errorf("unknown ZCL cluster 0x%04X", clusterID)
	}

	if len(fields) == 0 {
		return nil, fmt.Errorf("no decodable ZCL attributes")
	}

	fields["cluster_id"] = fmt.Sprintf("0x%04X", clusterID)
	return &DecodedPayload{Format: "zigbee", Fields: fields}, nil
}

// BridgeGPSFullDecoder decodes the bridge/Android full position frame (0x50, 16 bytes, LE).
// Format: [0x50][lat:i32 LE microdeg][lon:i32 LE microdeg][alt:i16 LE m][hdg:u16 LE deg][spd:u16 LE cm/s][bat:u8 %]
type BridgeGPSFullDecoder struct{}

func (BridgeGPSFullDecoder) Name() string { return "gps_bridge_full" }

func (BridgeGPSFullDecoder) Decode(payload []byte) (*DecodedPayload, error) {
	if len(payload) < 16 || payload[0] != 0x50 {
		return nil, fmt.Errorf("not a bridge full GPS frame")
	}
	lat := float64(int32(payload[4])<<24|int32(payload[3])<<16|int32(payload[2])<<8|int32(payload[1])) / 1e6
	lon := float64(int32(payload[8])<<24|int32(payload[7])<<16|int32(payload[6])<<8|int32(payload[5])) / 1e6
	alt := int16(payload[9]) | int16(payload[10])<<8
	hdg := uint16(payload[11]) | uint16(payload[12])<<8
	spd := uint16(payload[13]) | uint16(payload[14])<<8
	bat := payload[15]

	return &DecodedPayload{Format: "gps_bridge_full", Fields: map[string]interface{}{
		"lat":         lat,
		"lon":         lon,
		"alt":         float64(alt),
		"heading":     float64(hdg),
		"speed_cm_s":  float64(spd),
		"battery_pct": float64(bat),
	}}, nil
}

// BridgeGPSDeltaDecoder decodes the bridge/Android delta position frame (0x44, 11 bytes, LE).
// Format: [0x44][dlat:i16 LE microdeg][dlon:i16 LE microdeg][dalt:i8 m][hdg:u16 LE deg][spd:u16 LE cm/s][bat:u8 %]
type BridgeGPSDeltaDecoder struct{}

func (BridgeGPSDeltaDecoder) Name() string { return "gps_bridge_delta" }

func (BridgeGPSDeltaDecoder) Decode(payload []byte) (*DecodedPayload, error) {
	if len(payload) < 11 || payload[0] != 0x44 {
		return nil, fmt.Errorf("not a bridge delta GPS frame")
	}
	dlat := int16(payload[1]) | int16(payload[2])<<8
	dlon := int16(payload[3]) | int16(payload[4])<<8
	dalt := int8(payload[5])
	hdg := uint16(payload[6]) | uint16(payload[7])<<8
	spd := uint16(payload[8]) | uint16(payload[9])<<8
	bat := payload[10]

	return &DecodedPayload{Format: "gps_bridge_delta", Fields: map[string]interface{}{
		"delta_lat":   float64(dlat) / 1e6,
		"delta_lon":   float64(dlon) / 1e6,
		"delta_alt":   float64(dalt),
		"heading":     float64(hdg),
		"speed_cm_s":  float64(spd),
		"battery_pct": float64(bat),
	}}, nil
}

// CannedDecoder decodes the 0xCA canned military brevity codebook.
// Format: [0xCA][1-byte message ID] — 2 bytes total, maps to predefined phrases.
type CannedDecoder struct{}

func (CannedDecoder) Name() string { return "canned" }

func (CannedDecoder) Decode(payload []byte) (*DecodedPayload, error) {
	if len(payload) < 2 || payload[0] != 0xCA {
		return nil, fmt.Errorf("not a canned message")
	}
	id := int(payload[1])
	text, ok := cannedMessages[id]
	if !ok {
		return nil, fmt.Errorf("unknown canned message ID %d", id)
	}
	return &DecodedPayload{Format: "canned", Fields: map[string]interface{}{
		"message_id": id,
		"text":       text,
	}}, nil
}

// cannedMessages is the 30-entry military brevity codebook shared with bridge and Android.
var cannedMessages = map[int]string{
	1: "Copy", 2: "Roger", 3: "Negative", 4: "Affirmative", 5: "Stand by",
	6: "All clear", 7: "Moving out", 8: "Returning to base", 9: "Position confirmed",
	10: "Mission complete", 11: "Need resupply", 12: "Requesting backup",
	13: "Medical emergency", 14: "Evacuate immediately", 15: "Hold position",
	16: "Proceed to waypoint", 17: "Enemy contact", 18: "All personnel accounted for",
	19: "Weather deteriorating", 20: "Low battery warning", 21: "Signal lost",
	22: "Relay message", 23: "Check in", 24: "Going silent", 25: "SOS — need immediate help",
	26: "Camp established", 27: "Trail blocked — rerouting", 28: "Water source found",
	29: "Shelter located", 30: "Search area clear — no findings",
}

// RawDecoder is the fallback — returns raw hex.
type RawDecoder struct{}

func (RawDecoder) Name() string { return "raw" }

func (RawDecoder) Decode(payload []byte) (*DecodedPayload, error) {
	return &DecodedPayload{
		Format: "raw",
		Fields: map[string]interface{}{"raw_hex": fmt.Sprintf("%x", payload)},
	}, nil
}
