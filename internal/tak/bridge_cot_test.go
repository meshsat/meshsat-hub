package tak

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/protocol"
)

func TestBuildBridgeEvent(t *testing.T) {
	birth := protocol.BridgeBirth{
		Protocol:    protocol.ProtocolVersion,
		BridgeID:    "mule01",
		Version:     "0.2.0",
		Hostname:    "mule01.local",
		Mode:        "direct",
		CoTType:     protocol.CoTBridge,
		CoTCallsign: "MULE-01",
		Location: &protocol.Location{
			Lat:    52.3676,
			Lon:    4.9041,
			Alt:    10.0,
			Source: "fixed",
		},
		Interfaces: []protocol.InterfaceInfo{
			{Name: "mesh_0", Type: "meshtastic", Status: "online"},
			{Name: "iridium_0", Type: "iridium_imt", Status: "online"},
		},
		UptimeSec: 3600,
		Timestamp: time.Now(),
	}

	ev := BuildBridgeEvent(birth, 600)

	// Verify XML serialization.
	data, err := xml.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	// Parse back.
	var parsed CotEvent
	if err := xml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	// Check fields.
	if parsed.Version != "2.0" {
		t.Errorf("version = %q, want 2.0", parsed.Version)
	}
	if parsed.UID != "meshsat-bridge-mule01" {
		t.Errorf("uid = %q, want meshsat-bridge-mule01", parsed.UID)
	}
	if parsed.Type != protocol.CoTBridge {
		t.Errorf("type = %q, want %q", parsed.Type, protocol.CoTBridge)
	}
	if parsed.How != "m-g" {
		t.Errorf("how = %q, want m-g", parsed.How)
	}
	if parsed.Point.Lat != 52.3676 {
		t.Errorf("lat = %f, want 52.3676", parsed.Point.Lat)
	}
	if parsed.Point.Lon != 4.9041 {
		t.Errorf("lon = %f, want 4.9041", parsed.Point.Lon)
	}
	if parsed.Point.Hae != 10.0 {
		t.Errorf("hae = %f, want 10.0", parsed.Point.Hae)
	}
	if parsed.Point.Ce != 10.0 {
		t.Errorf("ce = %f, want 10.0 (fixed position)", parsed.Point.Ce)
	}
	if parsed.Detail == nil {
		t.Fatal("detail is nil")
	}
	if parsed.Detail.Contact == nil || parsed.Detail.Contact.Callsign != "MULE-01" {
		t.Errorf("callsign = %q, want MULE-01", parsed.Detail.Contact.Callsign)
	}
	if parsed.Detail.Group == nil || parsed.Detail.Group.Name != "Cyan" {
		t.Errorf("group name = %q, want Cyan", parsed.Detail.Group.Name)
	}

	// Check remarks contain expected metadata.
	if parsed.Detail.Remarks == nil {
		t.Fatal("remarks is nil")
	}
	remarks := parsed.Detail.Remarks.Text
	for _, want := range []string{"v0.2.0", "host=mule01.local", "mode=direct", "mesh_0(online)", "up=3600s"} {
		if !strings.Contains(remarks, want) {
			t.Errorf("remarks %q missing %q", remarks, want)
		}
	}

	// Verify stale time is in the future.
	stale, err := time.Parse(cotTimeFormat, parsed.Stale)
	if err != nil {
		t.Fatalf("parse stale time: %v", err)
	}
	if stale.Before(time.Now().UTC()) {
		t.Error("stale time should be in the future")
	}
}

func TestBuildBridgeEvent_NoLocation(t *testing.T) {
	birth := protocol.BridgeBirth{
		BridgeID:    "nomad01",
		CoTType:     protocol.CoTBridge,
		CoTCallsign: "NOMAD-01",
		// No Location field.
	}

	ev := BuildBridgeEvent(birth, 300)

	if ev.Point.Lat != 0 {
		t.Errorf("lat = %f, want 0 (no location)", ev.Point.Lat)
	}
	if ev.Point.Lon != 0 {
		t.Errorf("lon = %f, want 0 (no location)", ev.Point.Lon)
	}
	if ev.UID != "meshsat-bridge-nomad01" {
		t.Errorf("uid = %q, want meshsat-bridge-nomad01", ev.UID)
	}
}

func TestBuildBridgeEvent_DefaultCallsignAndType(t *testing.T) {
	birth := protocol.BridgeBirth{
		BridgeID: "test01",
		// No CoTType or CoTCallsign.
	}

	ev := BuildBridgeEvent(birth, 300)

	if ev.Type != protocol.CoTBridge {
		t.Errorf("type = %q, want %q (default)", ev.Type, protocol.CoTBridge)
	}
	if ev.Detail.Contact.Callsign != "BRIDGE-test01" {
		t.Errorf("callsign = %q, want BRIDGE-test01 (default)", ev.Detail.Contact.Callsign)
	}
}

func TestBuildDeviceBirthEvent(t *testing.T) {
	device := protocol.DeviceBirth{
		Protocol:    protocol.ProtocolVersion,
		DeviceID:    "!abc1234",
		BridgeID:    "mule01",
		Type:        protocol.DeviceMeshtastic,
		Label:       "Mesh Node 1",
		Hardware:    "heltec-v3",
		Firmware:    "2.3.0",
		CoTType:     protocol.CoTMeshNode,
		CoTCallsign: "MESH-1234",
		Position: &protocol.Location{
			Lat:    51.5074,
			Lon:    -0.1278,
			Alt:    20.0,
			Source: "GPS",
		},
		Timestamp: time.Now(),
	}

	ev := BuildDeviceBirthEvent(device, 600)

	data, err := xml.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var parsed CotEvent
	if err := xml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if parsed.UID != "meshsat-device-!abc1234" {
		t.Errorf("uid = %q, want meshsat-device-!abc1234", parsed.UID)
	}
	if parsed.Type != protocol.CoTMeshNode {
		t.Errorf("type = %q, want %q", parsed.Type, protocol.CoTMeshNode)
	}
	if parsed.Point.Lat != 51.5074 {
		t.Errorf("lat = %f, want 51.5074", parsed.Point.Lat)
	}
	if parsed.Point.Ce != 10.0 {
		t.Errorf("ce = %f, want 10.0 (has GPS)", parsed.Point.Ce)
	}
	if parsed.Detail.Contact.Callsign != "MESH-1234" {
		t.Errorf("callsign = %q, want MESH-1234", parsed.Detail.Contact.Callsign)
	}

	remarks := parsed.Detail.Remarks.Text
	for _, want := range []string{"type=meshtastic_node", "hw=heltec-v3", "fw=2.3.0", "bridge=mule01"} {
		if !strings.Contains(remarks, want) {
			t.Errorf("remarks %q missing %q", remarks, want)
		}
	}
}

func TestBuildDeviceBirthEvent_NoPosition(t *testing.T) {
	device := protocol.DeviceBirth{
		DeviceID:    "sat-modem-1",
		BridgeID:    "mule01",
		Type:        protocol.DeviceIridiumSBD,
		CoTType:     protocol.CoTSatModem,
		CoTCallsign: "SAT-1",
		// No Position.
	}

	ev := BuildDeviceBirthEvent(device, 600)

	if ev.Point.Ce != 100.0 {
		t.Errorf("ce = %f, want 100.0 (no position)", ev.Point.Ce)
	}
	if ev.Point.Lat != 0 || ev.Point.Lon != 0 {
		t.Errorf("lat/lon = %f,%f, want 0,0 (no position)", ev.Point.Lat, ev.Point.Lon)
	}
}

func TestBuildBridgeHealthEvent(t *testing.T) {
	birth := &protocol.BridgeBirth{
		BridgeID:    "mule01",
		CoTType:     protocol.CoTBridge,
		CoTCallsign: "MULE-01",
		Location: &protocol.Location{
			Lat: 52.3676, Lon: 4.9041, Alt: 10.0, Source: "fixed",
		},
	}

	health := protocol.BridgeHealth{
		Protocol:  protocol.ProtocolVersion,
		BridgeID:  "mule01",
		UptimeSec: 7200,
		CPUPct:    25.5,
		MemPct:    62.3,
		DiskPct:   45.0,
		Interfaces: []protocol.InterfaceHealth{
			{Name: "mesh_0", Status: "online", HealthScore: 95},
			{Name: "iridium_0", Status: "offline"},
		},
		Timestamp: time.Now(),
	}

	ev := BuildBridgeHealthEvent(health, birth, 600)

	data, err := xml.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var parsed CotEvent
	if err := xml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if parsed.UID != "meshsat-bridge-mule01" {
		t.Errorf("uid = %q, want meshsat-bridge-mule01", parsed.UID)
	}
	if parsed.Type != protocol.CoTBridge {
		t.Errorf("type = %q, want %q", parsed.Type, protocol.CoTBridge)
	}
	if parsed.Detail.Contact.Callsign != "MULE-01" {
		t.Errorf("callsign = %q, want MULE-01 (from birth)", parsed.Detail.Contact.Callsign)
	}
	if parsed.Point.Lat != 52.3676 {
		t.Errorf("lat = %f, want 52.3676 (from birth)", parsed.Point.Lat)
	}

	remarks := parsed.Detail.Remarks.Text
	for _, want := range []string{"cpu=26%", "mem=62%", "disk=45%", "mesh_0(online)", "up=7200s"} {
		if !strings.Contains(remarks, want) {
			t.Errorf("remarks %q missing %q", remarks, want)
		}
	}
}

func TestBuildBridgeHealthEvent_NoBirth(t *testing.T) {
	health := protocol.BridgeHealth{
		BridgeID:  "unknown01",
		CPUPct:    50.0,
		MemPct:    70.0,
		DiskPct:   30.0,
		Timestamp: time.Now(),
	}

	ev := BuildBridgeHealthEvent(health, nil, 300)

	if ev.Type != protocol.CoTBridge {
		t.Errorf("type = %q, want %q (default)", ev.Type, protocol.CoTBridge)
	}
	if ev.Detail.Contact.Callsign != "BRIDGE-unknown01" {
		t.Errorf("callsign = %q, want BRIDGE-unknown01 (default)", ev.Detail.Contact.Callsign)
	}
	if ev.Point.Lat != 0 || ev.Point.Lon != 0 {
		t.Errorf("lat/lon = %f,%f, want 0,0 (no birth)", ev.Point.Lat, ev.Point.Lon)
	}
}

func TestBridgeCoTType_MatchesProtocol(t *testing.T) {
	// Verify bridge CoT type constant is the expected MIL-STD-2525 code.
	if protocol.CoTBridge != "a-f-G-U-C-I" {
		t.Errorf("CoTBridge = %q, want a-f-G-U-C-I", protocol.CoTBridge)
	}
}

func TestExtractBridgeIDFromCoTTopic(t *testing.T) {
	tests := []struct {
		topic string
		want  string
	}{
		{"meshsat/bridge/mule01/birth", "mule01"},
		{"meshsat/bridge/mule01/health", "mule01"},
		{"meshsat/bridge/mule01/death", "mule01"},
		{"meshsat/bridge/mule01/device/abc/birth", "mule01"},
		{"meshsat/device/123/position", ""},
		{"other/topic", ""},
	}

	for _, tt := range tests {
		got := extractBridgeIDFromCoTTopic(tt.topic)
		if got != tt.want {
			t.Errorf("extractBridgeIDFromCoTTopic(%q) = %q, want %q", tt.topic, got, tt.want)
		}
	}
}

func TestExtractDeviceIDFromCoTTopic(t *testing.T) {
	tests := []struct {
		topic string
		want  string
	}{
		{"meshsat/bridge/mule01/device/abc123/birth", "abc123"},
		{"meshsat/bridge/mule01/device/xyz/death", "xyz"},
		{"meshsat/bridge/mule01/birth", ""},
		{"other/topic", ""},
	}

	for _, tt := range tests {
		got := extractDeviceIDFromCoTTopic(tt.topic)
		if got != tt.want {
			t.Errorf("extractDeviceIDFromCoTTopic(%q) = %q, want %q", tt.topic, got, tt.want)
		}
	}
}
