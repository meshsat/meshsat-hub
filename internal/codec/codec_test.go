package codec

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/cubeos-app/meshsat-hub/internal/geo"
)

func TestRegistry_List(t *testing.T) {
	r := NewRegistry()
	codecs := r.List()
	if len(codecs) != 7 {
		t.Fatalf("expected 7 codecs, got %d", len(codecs))
	}
	names := map[string]bool{}
	for _, c := range codecs {
		names[c.Name] = true
	}
	for _, want := range []string{"gps_bridge_full", "gps_bridge_delta", "gps", "canned", "json", "zigbee", "raw"} {
		if !names[want] {
			t.Errorf("missing codec %q", want)
		}
	}
}

func TestJSONDecoder(t *testing.T) {
	r := NewRegistry()

	payload := []byte(`{"temperature": 22.5, "humidity": 65.2}`)
	result, err := r.Decode("json", payload)
	if err != nil {
		t.Fatal(err)
	}
	if result.Format != "json" {
		t.Errorf("format = %s, want json", result.Format)
	}
	if result.Fields["temperature"] != 22.5 {
		t.Errorf("temperature = %v", result.Fields["temperature"])
	}
}

func TestJSONDecoder_NotJSON(t *testing.T) {
	r := NewRegistry()
	_, err := r.Decode("json", []byte("not json"))
	if err == nil {
		t.Error("expected error for non-JSON")
	}
}

func TestGPSDecoder_ValidFrame(t *testing.T) {
	r := NewRegistry()

	// Construct a hub GPS frame using geo.EncodeGPS for correct byte layout.
	payload := geo.EncodeGPS(&geo.GPSPosition{Lat: 52.3676, Lon: 4.9041})

	result, err := r.Decode("gps", payload)
	if err != nil {
		t.Fatal(err)
	}
	if result.Format != "gps" {
		t.Errorf("format = %s, want gps", result.Format)
	}
	gotLat := result.Fields["lat"].(float64)
	if math.Abs(gotLat-52.3676) > 0.001 {
		t.Errorf("lat = %f, want ~52.3676", gotLat)
	}
}

func TestGPSDecoder_NotGPS(t *testing.T) {
	r := NewRegistry()
	_, err := r.Decode("gps", []byte("hello"))
	if err == nil {
		t.Error("expected error for non-GPS data")
	}
}

func TestZigBeeDecoder_Temperature(t *testing.T) {
	r := NewRegistry()

	// ZCL temperature: frame_ctrl(1) + seq(1) + cluster 0x0402(2 LE) + attr_id(1) + value(2 LE)
	// Temperature: 22.50°C = 2250 = 0x08CA
	payload := []byte{0x01, 0x01, 0x02, 0x04, 0x00, 0xCA, 0x08}

	result, err := r.Decode("zigbee", payload)
	if err != nil {
		t.Fatal(err)
	}
	if result.Format != "zigbee" {
		t.Errorf("format = %s, want zigbee", result.Format)
	}
	temp := result.Fields["temperature_c"].(float64)
	if math.Abs(temp-22.50) > 0.01 {
		t.Errorf("temperature = %f, want 22.50", temp)
	}
}

func TestZigBeeDecoder_Humidity(t *testing.T) {
	r := NewRegistry()

	// Humidity cluster 0x0405, value 6520 = 65.20%
	payload := []byte{0x01, 0x01, 0x05, 0x04, 0x00, 0x78, 0x19}

	result, err := r.Decode("zigbee", payload)
	if err != nil {
		t.Fatal(err)
	}
	hum := result.Fields["humidity_pct"].(float64)
	if math.Abs(hum-65.20) > 0.1 {
		t.Errorf("humidity = %f, want ~65.20", hum)
	}
}

func TestAutoDetect_JSON(t *testing.T) {
	r := NewRegistry()
	result, err := r.Decode("auto", []byte(`{"sensor":"dht22","temp":21.3}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Format != "json" {
		t.Errorf("auto-detect format = %s, want json", result.Format)
	}
}

func TestAutoDetect_Fallback(t *testing.T) {
	r := NewRegistry()
	result, err := r.Decode("auto", []byte{0xDE, 0xAD, 0xBE, 0xEF})
	if err != nil {
		t.Fatal(err)
	}
	if result.Format != "raw" {
		t.Errorf("auto-detect fallback format = %s, want raw", result.Format)
	}
}

func TestRawDecoder(t *testing.T) {
	r := NewRegistry()
	result, err := r.Decode("raw", []byte{0xCA, 0xFE})
	if err != nil {
		t.Fatal(err)
	}
	if result.Fields["raw_hex"] != "cafe" {
		t.Errorf("raw_hex = %v, want cafe", result.Fields["raw_hex"])
	}
}

func TestBridgeGPSFullDecoder(t *testing.T) {
	r := NewRegistry()
	// Encode 52.367600° lat, 4.904100° lon as microdegrees (×1e6), little-endian
	lat := int32(52367600)
	lon := int32(4904100)
	payload := []byte{
		0x50,                                                        // magic
		byte(lat), byte(lat >> 8), byte(lat >> 16), byte(lat >> 24), // lat LE
		byte(lon), byte(lon >> 8), byte(lon >> 16), byte(lon >> 24), // lon LE
		0x64, 0x00, // alt = 100m
		0x68, 0x01, // heading = 360°
		0xE8, 0x03, // speed = 1000 cm/s
		0x50, // battery = 80%
	}

	result, err := r.Decode("gps_bridge_full", payload)
	if err != nil {
		t.Fatal(err)
	}
	if result.Format != "gps_bridge_full" {
		t.Errorf("format = %s", result.Format)
	}
	gotLat := result.Fields["lat"].(float64)
	if math.Abs(gotLat-52.3676) > 0.001 {
		t.Errorf("lat = %f, want ~52.3676", gotLat)
	}
	gotLon := result.Fields["lon"].(float64)
	if math.Abs(gotLon-4.9041) > 0.001 {
		t.Errorf("lon = %f, want ~4.9041", gotLon)
	}
	if result.Fields["battery_pct"].(float64) != 80 {
		t.Errorf("battery = %v, want 80", result.Fields["battery_pct"])
	}
}

func TestBridgeGPSDeltaDecoder(t *testing.T) {
	r := NewRegistry()
	// Delta: +100 microdeg lat, -50 microdeg lon
	payload := []byte{
		0x44,       // magic
		0x64, 0x00, // dlat = 100
		0xCE, 0xFF, // dlon = -50 (0xFFCE = -50 as int16)
		0x05,       // dalt = +5m
		0xB4, 0x00, // heading = 180°
		0xC8, 0x00, // speed = 200 cm/s
		0x5A, // battery = 90%
	}

	result, err := r.Decode("gps_bridge_delta", payload)
	if err != nil {
		t.Fatal(err)
	}
	if result.Format != "gps_bridge_delta" {
		t.Errorf("format = %s", result.Format)
	}
	if result.Fields["delta_alt"].(float64) != 5 {
		t.Errorf("delta_alt = %v, want 5", result.Fields["delta_alt"])
	}
}

func TestBridgeGPS_AutoDetect(t *testing.T) {
	r := NewRegistry()
	lat := int32(52367600)
	lon := int32(4904100)
	payload := make([]byte, 16)
	payload[0] = 0x50
	payload[1] = byte(lat)
	payload[2] = byte(lat >> 8)
	payload[3] = byte(lat >> 16)
	payload[4] = byte(lat >> 24)
	payload[5] = byte(lon)
	payload[6] = byte(lon >> 8)
	payload[7] = byte(lon >> 16)
	payload[8] = byte(lon >> 24)

	result, err := r.Decode("auto", payload)
	if err != nil {
		t.Fatal(err)
	}
	if result.Format != "gps_bridge_full" {
		t.Errorf("auto-detect: format = %s, want gps_bridge_full", result.Format)
	}
}

func TestCannedDecoder(t *testing.T) {
	r := NewRegistry()
	// 0xCA + ID 25 = "SOS — need immediate help"
	result, err := r.Decode("canned", []byte{0xCA, 25})
	if err != nil {
		t.Fatal(err)
	}
	if result.Format != "canned" {
		t.Errorf("format = %s", result.Format)
	}
	if result.Fields["text"] != "SOS — need immediate help" {
		t.Errorf("text = %v", result.Fields["text"])
	}
	if result.Fields["message_id"].(int) != 25 {
		t.Errorf("message_id = %v", result.Fields["message_id"])
	}
}

func TestCannedDecoder_AllEntries(t *testing.T) {
	r := NewRegistry()
	for id := 1; id <= 30; id++ {
		result, err := r.Decode("canned", []byte{0xCA, byte(id)})
		if err != nil {
			t.Errorf("ID %d: %v", id, err)
			continue
		}
		if result.Fields["text"] == "" {
			t.Errorf("ID %d: empty text", id)
		}
	}
}

func TestCannedDecoder_InvalidID(t *testing.T) {
	r := NewRegistry()
	_, err := r.Decode("canned", []byte{0xCA, 99})
	if err == nil {
		t.Error("expected error for unknown canned ID")
	}
}

func TestCannedDecoder_AutoDetect(t *testing.T) {
	r := NewRegistry()
	result, err := r.Decode("auto", []byte{0xCA, 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Format != "canned" {
		t.Errorf("auto-detect: format = %s, want canned", result.Format)
	}
	if result.Fields["text"] != "Copy" {
		t.Errorf("text = %v, want Copy", result.Fields["text"])
	}
}

func TestUnknownFormat(t *testing.T) {
	r := NewRegistry()
	_, err := r.Decode("unknown_format", []byte("test"))
	if err == nil {
		t.Error("expected error for unknown format")
	}
}

func TestDecodedPayload_JSON(t *testing.T) {
	p := &DecodedPayload{
		Format: "json",
		Fields: map[string]interface{}{"temp": 22.5},
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Error("expected non-empty JSON")
	}
}
