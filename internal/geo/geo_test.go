package geo

import (
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
