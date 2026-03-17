package tak

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func TestBuildPositionEvent(t *testing.T) {
	ev := BuildPositionEvent("meshsat-test", "HUB-TEST", 52.3676, 4.9041, 10.0, 300, "gps")

	if ev.Type != TypePosition {
		t.Errorf("type: got %q, want %q", ev.Type, TypePosition)
	}
	if ev.UID != "meshsat-test" {
		t.Errorf("uid: got %q", ev.UID)
	}
	if ev.Point.Lat != 52.3676 {
		t.Errorf("lat: got %f", ev.Point.Lat)
	}
	if ev.Detail == nil || ev.Detail.Contact == nil {
		t.Fatal("detail/contact nil")
	}
	if ev.Detail.Contact.Callsign != "HUB-TEST" {
		t.Errorf("callsign: got %q", ev.Detail.Contact.Callsign)
	}

	// Stale ~300s after time
	evTime, _ := time.Parse(cotTimeFormat, ev.Time)
	evStale, _ := time.Parse(cotTimeFormat, ev.Stale)
	if d := evStale.Sub(evTime); d < 299*time.Second || d > 301*time.Second {
		t.Errorf("stale offset: %v", d)
	}
}

func TestBuildPositionEvent_IridiumCEP(t *testing.T) {
	ev := BuildPositionEvent("meshsat-test", "HUB-TEST", 52.0, 4.0, 0, 600, "iridium_cep")

	if ev.Point.Ce != 10000.0 {
		t.Errorf("CE for iridium_cep: got %f, want 10000", ev.Point.Ce)
	}
	if ev.Detail.Precision.GeoPointSrc != "Iridium-CEP" {
		t.Errorf("geopointsrc: got %q", ev.Detail.Precision.GeoPointSrc)
	}
}

func TestBuildSOSEvent(t *testing.T) {
	ev := BuildSOSEvent("meshsat-sos", "HUB-SOS", 52.0, 4.0, 600)

	if ev.Type != TypePosition {
		t.Errorf("type: got %q", ev.Type)
	}
	if ev.Detail == nil || ev.Detail.Emergency == nil {
		t.Fatal("emergency nil")
	}
	if ev.Detail.Emergency.Type != "911 Alert" {
		t.Errorf("emergency type: got %q", ev.Detail.Emergency.Type)
	}
}

func TestBuildTelemetryEvent(t *testing.T) {
	ev := BuildTelemetryEvent("meshsat-tel", "HUB-TEL", 52.0, 4.0, 300, "temp=22C")

	if ev.Type != TypeSensor {
		t.Errorf("type: got %q, want %q", ev.Type, TypeSensor)
	}
	if ev.Detail == nil || ev.Detail.Remarks == nil {
		t.Fatal("remarks nil")
	}
	if !strings.Contains(ev.Detail.Remarks.Text, "temp=22C") {
		t.Errorf("remarks: got %q", ev.Detail.Remarks.Text)
	}
}

func TestBuildChatEvent(t *testing.T) {
	ev := BuildChatEvent("meshsat-chat", "HUB-CHAT", "Hello from satellite", 300)

	if ev.Type != TypeChat {
		t.Errorf("type: got %q, want %q", ev.Type, TypeChat)
	}
	if ev.Detail == nil || ev.Detail.Remarks == nil {
		t.Fatal("remarks nil")
	}
	if ev.Detail.Remarks.Text != "Hello from satellite" {
		t.Errorf("remarks text: got %q", ev.Detail.Remarks.Text)
	}
}

func TestMarshalParsRoundtrip(t *testing.T) {
	original := BuildSOSEvent("roundtrip-001", "RT-1", 51.5074, -0.1278, 600)
	data, err := MarshalCotEvent(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	parsed, err := ParseCotEvent(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.UID != original.UID {
		t.Errorf("uid: got %q, want %q", parsed.UID, original.UID)
	}
	if parsed.Detail == nil || parsed.Detail.Emergency == nil {
		t.Fatal("emergency lost in roundtrip")
	}
}

func TestMarshalCotEvent_ValidXML(t *testing.T) {
	ev := BuildPositionEvent("xml-test", "TEST", 52.0, 4.0, 0, 300, "gps")
	data, err := MarshalCotEvent(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	s := string(data)
	if !strings.Contains(s, `<event`) {
		t.Error("missing <event>")
	}
	if !strings.Contains(s, `type="a-f-G-U-C"`) {
		t.Error("missing type attr")
	}
	if !strings.Contains(s, `<point`) {
		t.Error("missing <point>")
	}

	// Verify it's valid XML
	var check CotEvent
	if err := xml.Unmarshal(data, &check); err != nil {
		t.Errorf("invalid XML: %v", err)
	}
}
