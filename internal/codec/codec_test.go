package codec

import (
	"encoding/json"
	"math"
	"testing"
)

func TestRegistry_List(t *testing.T) {
	r := NewRegistry()
	codecs := r.List()
	if len(codecs) != 4 {
		t.Fatalf("expected 4 codecs, got %d", len(codecs))
	}
	names := map[string]bool{}
	for _, c := range codecs {
		names[c.Name] = true
	}
	for _, want := range []string{"gps", "json", "zigbee", "raw"} {
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

	// Construct a GPS frame: 0x47 + lat(52.3676°) + lon(4.9041°) + padding
	lat := int32(52.3676 * 1e7)
	lon := int32(4.9041 * 1e7)
	payload := []byte{
		0x47, // magic
		byte(lat >> 24), byte(lat >> 16), byte(lat >> 8), byte(lat),
		byte(lon >> 24), byte(lon >> 16), byte(lon >> 8), byte(lon),
		0x00, // padding
	}

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
