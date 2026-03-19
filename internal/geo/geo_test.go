package geo

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

// --- Simplify tests ---

func TestSimplify_Empty(t *testing.T) {
	result := Simplify(nil, 0.001)
	if result != nil {
		t.Fatalf("expected nil, got %v", result)
	}
}

func TestSimplify_TwoPoints(t *testing.T) {
	pts := []Point{{52.0, 4.0}, {52.1, 4.1}}
	result := Simplify(pts, 0.001)
	if len(result) != 2 {
		t.Fatalf("expected 2 points, got %d", len(result))
	}
}

func TestSimplify_StraightLine(t *testing.T) {
	// Points along a straight line should reduce to just endpoints.
	pts := []Point{
		{52.0, 4.0}, {52.01, 4.01}, {52.02, 4.02},
		{52.03, 4.03}, {52.04, 4.04}, {52.05, 4.05},
	}
	result := Simplify(pts, 0.001)
	if len(result) != 2 {
		t.Fatalf("straight line: expected 2 points, got %d", len(result))
	}
}

func TestSimplify_ZigZag(t *testing.T) {
	// Zig-zag path should retain more points.
	pts := []Point{
		{52.0, 4.0}, {52.01, 4.05}, {52.02, 4.0},
		{52.03, 4.05}, {52.04, 4.0},
	}
	result := Simplify(pts, 0.001)
	if len(result) < 3 {
		t.Fatalf("zig-zag: expected >=3 points, got %d", len(result))
	}
}

func TestHaversineDistance(t *testing.T) {
	// Amsterdam to Rotterdam: ~58 km
	a := Point{52.3676, 4.9041}
	b := Point{51.9225, 4.4792}
	d := HaversineDistance(a, b)
	if d < 50000 || d > 65000 {
		t.Fatalf("expected ~58km, got %.0f m", d)
	}
}

// --- GPS codec tests ---

func TestCodecRoundTrip_AllFields(t *testing.T) {
	ts := time.Date(2026, 3, 18, 10, 30, 0, 0, time.UTC)
	orig := &GPSPosition{
		Lat:        52.3676123,
		Lon:        4.9041567,
		Alt:        150,
		Speed:      3.45,
		Heading:    275.50,
		Sats:       12,
		Timestamp:  ts,
		HasAlt:     true,
		HasSpeed:   true,
		HasHeading: true,
		HasSats:    true,
	}

	data := EncodeGPS(orig)
	decoded, err := DecodeGPS(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if math.Abs(decoded.Lat-orig.Lat) > 0.0000002 {
		t.Errorf("lat: got %f, want %f", decoded.Lat, orig.Lat)
	}
	if math.Abs(decoded.Lon-orig.Lon) > 0.0000002 {
		t.Errorf("lon: got %f, want %f", decoded.Lon, orig.Lon)
	}
	if decoded.Alt != orig.Alt {
		t.Errorf("alt: got %f, want %f", decoded.Alt, orig.Alt)
	}
	if math.Abs(decoded.Speed-orig.Speed) > 0.01 {
		t.Errorf("speed: got %f, want %f", decoded.Speed, orig.Speed)
	}
	if math.Abs(decoded.Heading-orig.Heading) > 0.01 {
		t.Errorf("heading: got %f, want %f", decoded.Heading, orig.Heading)
	}
	if decoded.Sats != orig.Sats {
		t.Errorf("sats: got %d, want %d", decoded.Sats, orig.Sats)
	}
	if !decoded.Timestamp.Equal(orig.Timestamp) {
		t.Errorf("timestamp: got %v, want %v", decoded.Timestamp, orig.Timestamp)
	}
}

func TestCodecRoundTrip_LatLonOnly(t *testing.T) {
	orig := &GPSPosition{
		Lat: -33.8688,
		Lon: 151.2093,
	}

	data := EncodeGPS(orig)
	if len(data) != 10 { // magic + flags + 4 lat + 4 lon
		t.Fatalf("expected 10 bytes, got %d", len(data))
	}

	decoded, err := DecodeGPS(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if math.Abs(decoded.Lat-orig.Lat) > 0.0000002 {
		t.Errorf("lat: got %f, want %f", decoded.Lat, orig.Lat)
	}
	if math.Abs(decoded.Lon-orig.Lon) > 0.0000002 {
		t.Errorf("lon: got %f, want %f", decoded.Lon, orig.Lon)
	}
	if decoded.HasAlt || decoded.HasSpeed || decoded.HasHeading || decoded.HasSats {
		t.Error("no optional fields should be set")
	}
}

func TestDecodeGPS_NotGPSFrame(t *testing.T) {
	_, err := DecodeGPS([]byte{0x00, 0x00, 0, 0, 0, 0, 0, 0, 0, 0})
	if err != ErrNotGPSFrame {
		t.Fatalf("expected ErrNotGPSFrame, got %v", err)
	}
}

func TestDecodeGPS_TooShort(t *testing.T) {
	_, err := DecodeGPS([]byte{0xA5})
	if err != ErrTooShort {
		t.Fatalf("expected ErrTooShort, got %v", err)
	}
}

func TestIsGPSFrame(t *testing.T) {
	if IsGPSFrame([]byte{0x00, 0x00}) {
		t.Error("should not detect non-GPS frame")
	}
	if !IsGPSFrame([]byte{0xA5, 0x00, 0, 0, 0, 0, 0, 0, 0, 0}) {
		t.Error("should detect GPS frame")
	}
}

// --- Bridge/Android GPS codec tests ---

func TestDecodeBridgeGPSFull(t *testing.T) {
	// Encode a known position in bridge format: 0x50, LE, microdegrees (×1e6).
	// Lat=52.367612, Lon=4.904157 → lat_micro=52367612, lon_micro=4904157
	buf := make([]byte, 16)
	buf[0] = 0x50
	binary.LittleEndian.PutUint32(buf[1:5], uint32(int32(52367612)))
	binary.LittleEndian.PutUint32(buf[5:9], uint32(int32(4904157)))
	binary.LittleEndian.PutUint16(buf[9:11], uint16(int16(150))) // alt
	binary.LittleEndian.PutUint16(buf[11:13], 275)               // heading
	binary.LittleEndian.PutUint16(buf[13:15], 345)               // speed cm/s
	buf[15] = 85                                                 // battery

	gps, err := DecodeBridgeGPSFull(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if math.Abs(gps.Lat-52.367612) > 0.000002 {
		t.Errorf("lat: got %f, want 52.367612", gps.Lat)
	}
	if math.Abs(gps.Lon-4.904157) > 0.000002 {
		t.Errorf("lon: got %f, want 4.904157", gps.Lon)
	}
	if gps.Alt != 150 {
		t.Errorf("alt: got %f, want 150", gps.Alt)
	}
	if gps.Heading != 275 {
		t.Errorf("heading: got %f, want 275", gps.Heading)
	}
	if math.Abs(gps.Speed-3.45) > 0.01 {
		t.Errorf("speed: got %f, want 3.45", gps.Speed)
	}
	if !gps.HasAlt || !gps.HasHeading || !gps.HasSpeed {
		t.Error("expected HasAlt, HasHeading, HasSpeed to be true")
	}
}

func TestDecodeBridgeGPSFull_TooShort(t *testing.T) {
	_, err := DecodeBridgeGPSFull([]byte{0x50, 0, 0, 0})
	if err != ErrTooShort {
		t.Fatalf("expected ErrTooShort, got %v", err)
	}
}

func TestDecodeBridgeGPSFull_WrongMagic(t *testing.T) {
	buf := make([]byte, 16)
	buf[0] = 0xFF
	_, err := DecodeBridgeGPSFull(buf)
	if err != ErrNotGPSFrame {
		t.Fatalf("expected ErrNotGPSFrame, got %v", err)
	}
}

func TestDecodeBridgeGPSDelta(t *testing.T) {
	prev := &GPSPosition{
		Lat: 52.367612,
		Lon: 4.904157,
		Alt: 150,
	}

	// Delta: +100 microdeg lat, -50 microdeg lon, +5m alt, heading=280, speed=400 cm/s
	buf := make([]byte, 11)
	buf[0] = 0x44
	binary.LittleEndian.PutUint16(buf[1:3], uint16(int16(100))) // dlat
	binary.LittleEndian.PutUint16(buf[3:5], uint16(65486))      // dlon: int16(-50) as uint16
	buf[5] = byte(int8(5))                                      // dalt
	binary.LittleEndian.PutUint16(buf[6:8], 280)                // heading
	binary.LittleEndian.PutUint16(buf[8:10], 400)               // speed cm/s
	buf[10] = 90                                                // battery

	gps, err := DecodeBridgeGPSDelta(buf, prev)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	expectedLat := float64(int32(math.Round(52.367612*1e6))+100) / 1e6
	expectedLon := float64(int32(math.Round(4.904157*1e6))-50) / 1e6
	if math.Abs(gps.Lat-expectedLat) > 0.000002 {
		t.Errorf("lat: got %f, want %f", gps.Lat, expectedLat)
	}
	if math.Abs(gps.Lon-expectedLon) > 0.000002 {
		t.Errorf("lon: got %f, want %f", gps.Lon, expectedLon)
	}
	if gps.Alt != 155 {
		t.Errorf("alt: got %f, want 155", gps.Alt)
	}
	if gps.Heading != 280 {
		t.Errorf("heading: got %f, want 280", gps.Heading)
	}
	if math.Abs(gps.Speed-4.0) > 0.01 {
		t.Errorf("speed: got %f, want 4.0", gps.Speed)
	}
}

func TestDecodeBridgeGPSDelta_NoPrev(t *testing.T) {
	buf := make([]byte, 11)
	buf[0] = 0x44
	_, err := DecodeBridgeGPSDelta(buf, nil)
	if err == nil {
		t.Fatal("expected error for delta without previous position")
	}
}

func TestIsBridgeGPSFrame(t *testing.T) {
	if !IsBridgeGPSFrame([]byte{0x50, 0}) {
		t.Error("should detect 0x50 frame")
	}
	if !IsBridgeGPSFrame([]byte{0x44, 0}) {
		t.Error("should detect 0x44 frame")
	}
	if IsBridgeGPSFrame([]byte{0xA5, 0}) {
		t.Error("should not detect 0xA5 as bridge frame")
	}
	if IsBridgeGPSFrame(nil) {
		t.Error("should not detect nil")
	}
}

func TestIsAnyGPSFrame(t *testing.T) {
	if !IsAnyGPSFrame([]byte{0xA5, 0x00, 0, 0, 0, 0, 0, 0, 0, 0}) {
		t.Error("should detect hub 0xA5")
	}
	if !IsAnyGPSFrame([]byte{0x50, 0}) {
		t.Error("should detect bridge 0x50")
	}
	if !IsAnyGPSFrame([]byte{0x44, 0}) {
		t.Error("should detect bridge 0x44")
	}
	if IsAnyGPSFrame([]byte{0xFF}) {
		t.Error("should not detect unknown magic")
	}
}

func TestCodec_NegativeAltitude(t *testing.T) {
	orig := &GPSPosition{
		Lat:    31.5,
		Lon:    35.5,
		Alt:    -400, // Dead Sea
		HasAlt: true,
	}
	data := EncodeGPS(orig)
	decoded, err := DecodeGPS(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Alt != -400 {
		t.Errorf("alt: got %f, want -400", decoded.Alt)
	}
}
