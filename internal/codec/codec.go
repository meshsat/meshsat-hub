// Package codec provides a pluggable decoder framework for structured sensor
// payloads. Decoders are registered by name and selected per-device based on
// the device's payload_format configuration.
package codec

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
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
	// Register built-in decoders.
	r.Register(&GPSDecoder{})
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

// GPSDecoder decodes the compact GPS binary frame from internal/geo.
type GPSDecoder struct{}

func (GPSDecoder) Name() string { return "gps" }

func (GPSDecoder) Decode(payload []byte) (*DecodedPayload, error) {
	// GPS frame: starts with magic byte 0x47 ('G'), min 10 bytes.
	if len(payload) < 10 || payload[0] != 0x47 {
		return nil, fmt.Errorf("not a GPS frame")
	}

	// Basic GPS decode: lat (4 bytes, int32 * 1e-7), lon (4 bytes, int32 * 1e-7)
	lat := float64(int32(payload[1])<<24|int32(payload[2])<<16|int32(payload[3])<<8|int32(payload[4])) / 1e7
	lon := float64(int32(payload[5])<<24|int32(payload[6])<<16|int32(payload[7])<<8|int32(payload[8])) / 1e7

	fields := map[string]interface{}{
		"lat": lat,
		"lon": lon,
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

// RawDecoder is the fallback — returns raw hex.
type RawDecoder struct{}

func (RawDecoder) Name() string { return "raw" }

func (RawDecoder) Decode(payload []byte) (*DecodedPayload, error) {
	return &DecodedPayload{
		Format: "raw",
		Fields: map[string]interface{}{"raw_hex": fmt.Sprintf("%x", payload)},
	}, nil
}
