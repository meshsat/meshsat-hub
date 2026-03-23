package bridge

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

// encodeSatPosition replicates bridge-side encoding for round-trip testing.
func encodeSatPosition(bridgeID string, lat, lon float64, alt float32, source byte, ts time.Time) []byte {
	if len(bridgeID) > 16 {
		bridgeID = bridgeID[:16]
	}
	size := 4 + 1 + len(bridgeID) + 4 + 4 + 2 + 1 + 4
	buf := make([]byte, size)
	buf[0], buf[1], buf[2], buf[3] = 0x4D, 0x53, 1, SatMsgPosition
	off := 4
	buf[off] = byte(len(bridgeID))
	off++
	copy(buf[off:], bridgeID)
	off += len(bridgeID)
	binary.BigEndian.PutUint32(buf[off:], math.Float32bits(float32(lat)))
	off += 4
	binary.BigEndian.PutUint32(buf[off:], math.Float32bits(float32(lon)))
	off += 4
	binary.BigEndian.PutUint16(buf[off:], uint16(int16(alt)))
	off += 2
	buf[off] = source
	off++
	binary.BigEndian.PutUint32(buf[off:], uint32(ts.Unix()))
	return buf
}

// encodeSatSOS replicates bridge-side encoding for round-trip testing.
func encodeSatSOS(bridgeID, deviceID string, lat, lon float64, message string, ts time.Time) []byte {
	if len(bridgeID) > 16 {
		bridgeID = bridgeID[:16]
	}
	if len(deviceID) > 16 {
		deviceID = deviceID[:16]
	}
	if len(message) > 64 {
		message = message[:64]
	}
	size := 4 + 1 + len(bridgeID) + 1 + len(deviceID) + 4 + 4 + 1 + len(message) + 4
	buf := make([]byte, size)
	buf[0], buf[1], buf[2], buf[3] = 0x4D, 0x53, 1, SatMsgSOS
	off := 4
	buf[off] = byte(len(bridgeID))
	off++
	copy(buf[off:], bridgeID)
	off += len(bridgeID)
	buf[off] = byte(len(deviceID))
	off++
	copy(buf[off:], deviceID)
	off += len(deviceID)
	binary.BigEndian.PutUint32(buf[off:], math.Float32bits(float32(lat)))
	off += 4
	binary.BigEndian.PutUint32(buf[off:], math.Float32bits(float32(lon)))
	off += 4
	buf[off] = byte(len(message))
	off++
	copy(buf[off:], message)
	off += len(message)
	binary.BigEndian.PutUint32(buf[off:], uint32(ts.Unix()))
	return buf
}

// encodeSatHealth replicates bridge-side encoding for round-trip testing.
func encodeSatHealth(bridgeID string, uptimeSec uint32, cpuPct, memPct, diskPct byte, ifaces []SatIfaceStatus, ts time.Time) []byte {
	if len(bridgeID) > 16 {
		bridgeID = bridgeID[:16]
	}
	ifaceSize := 0
	for _, iface := range ifaces {
		name := iface.Name
		if len(name) > 16 {
			name = name[:16]
		}
		ifaceSize += 1 + len(name) + 2
	}
	size := 4 + 1 + len(bridgeID) + 4 + 1 + 1 + 1 + 1 + ifaceSize + 4
	buf := make([]byte, size)
	buf[0], buf[1], buf[2], buf[3] = 0x4D, 0x53, 1, SatMsgHealthSummary
	off := 4
	buf[off] = byte(len(bridgeID))
	off++
	copy(buf[off:], bridgeID)
	off += len(bridgeID)
	binary.BigEndian.PutUint32(buf[off:], uptimeSec)
	off += 4
	buf[off] = cpuPct
	off++
	buf[off] = memPct
	off++
	buf[off] = diskPct
	off++
	buf[off] = byte(len(ifaces))
	off++
	for _, iface := range ifaces {
		name := iface.Name
		if len(name) > 16 {
			name = name[:16]
		}
		buf[off] = byte(len(name))
		off++
		copy(buf[off:], name)
		off += len(name)
		if iface.Online {
			buf[off] = 1
		}
		off++
		buf[off] = iface.Signal
		off++
	}
	binary.BigEndian.PutUint32(buf[off:], uint32(ts.Unix()))
	return buf
}

func TestDecodeSatPosition_RoundTrip(t *testing.T) {
	ts := time.Date(2026, 3, 23, 12, 0, 0, 0, time.UTC)
	data := encodeSatPosition("mule01", 51.5074, -0.1278, 42, 1, ts)

	if !IsBridgeSatUplink(data) {
		t.Fatal("IsBridgeSatUplink should return true")
	}

	msgType, payload, err := DecodeSatUplink(data)
	if err != nil {
		t.Fatalf("DecodeSatUplink: %v", err)
	}
	if msgType != SatMsgPosition {
		t.Fatalf("expected type %d, got %d", SatMsgPosition, msgType)
	}

	bridgeID, lat, lon, alt, source, gotTS, err := DecodeSatPosition(payload)
	if err != nil {
		t.Fatalf("DecodeSatPosition: %v", err)
	}
	if bridgeID != "mule01" {
		t.Errorf("bridgeID: got %q, want %q", bridgeID, "mule01")
	}
	if diff := lat - 51.5074; diff > 0.001 || diff < -0.001 {
		t.Errorf("lat: got %f, want ~51.5074", lat)
	}
	if diff := lon - (-0.1278); diff > 0.001 || diff < -0.001 {
		t.Errorf("lon: got %f, want ~-0.1278", lon)
	}
	if alt != 42 {
		t.Errorf("alt: got %f, want 42", alt)
	}
	if source != 1 {
		t.Errorf("source: got %d, want 1", source)
	}
	if !gotTS.Equal(ts) {
		t.Errorf("timestamp: got %v, want %v", gotTS, ts)
	}
}

func TestDecodeSatSOS_RoundTrip(t *testing.T) {
	ts := time.Date(2026, 3, 23, 14, 30, 0, 0, time.UTC)
	data := encodeSatSOS("mule01", "!abcd1234", 40.7128, -74.006, "Emergency", ts)

	msgType, payload, err := DecodeSatUplink(data)
	if err != nil {
		t.Fatalf("DecodeSatUplink: %v", err)
	}
	if msgType != SatMsgSOS {
		t.Fatalf("expected type %d, got %d", SatMsgSOS, msgType)
	}

	bridgeID, deviceID, lat, lon, message, gotTS, err := DecodeSatSOS(payload)
	if err != nil {
		t.Fatalf("DecodeSatSOS: %v", err)
	}
	if bridgeID != "mule01" {
		t.Errorf("bridgeID: got %q", bridgeID)
	}
	if deviceID != "!abcd1234" {
		t.Errorf("deviceID: got %q", deviceID)
	}
	if diff := lat - 40.7128; diff > 0.001 || diff < -0.001 {
		t.Errorf("lat: got %f", lat)
	}
	if diff := lon - (-74.006); diff > 0.001 || diff < -0.001 {
		t.Errorf("lon: got %f", lon)
	}
	if message != "Emergency" {
		t.Errorf("message: got %q", message)
	}
	if !gotTS.Equal(ts) {
		t.Errorf("timestamp: got %v, want %v", gotTS, ts)
	}
}

func TestDecodeSatHealth_RoundTrip(t *testing.T) {
	ts := time.Date(2026, 3, 23, 16, 0, 0, 0, time.UTC)
	ifaces := []SatIfaceStatus{
		{Name: "mesh_0", Online: true, Signal: 0},
		{Name: "iridium_0", Online: true, Signal: 4},
	}
	data := encodeSatHealth("mule01", 86400, 45, 72, 33, ifaces, ts)

	msgType, payload, err := DecodeSatUplink(data)
	if err != nil {
		t.Fatalf("DecodeSatUplink: %v", err)
	}
	if msgType != SatMsgHealthSummary {
		t.Fatalf("expected type %d, got %d", SatMsgHealthSummary, msgType)
	}

	bridgeID, uptime, cpu, mem, disk, gotIfaces, gotTS, err := DecodeSatHealth(payload)
	if err != nil {
		t.Fatalf("DecodeSatHealth: %v", err)
	}
	if bridgeID != "mule01" {
		t.Errorf("bridgeID: got %q", bridgeID)
	}
	if uptime != 86400 {
		t.Errorf("uptime: got %d", uptime)
	}
	if cpu != 45 || mem != 72 || disk != 33 {
		t.Errorf("metrics: got %d/%d/%d", cpu, mem, disk)
	}
	if len(gotIfaces) != 2 {
		t.Fatalf("iface count: got %d, want 2", len(gotIfaces))
	}
	if gotIfaces[0].Name != "mesh_0" || !gotIfaces[0].Online {
		t.Errorf("iface[0]: got %+v", gotIfaces[0])
	}
	if gotIfaces[1].Name != "iridium_0" || gotIfaces[1].Signal != 4 {
		t.Errorf("iface[1]: got %+v", gotIfaces[1])
	}
	if !gotTS.Equal(ts) {
		t.Errorf("timestamp: got %v, want %v", gotTS, ts)
	}
}

func TestIsBridgeSatUplink_ValidAndInvalid(t *testing.T) {
	valid := []byte{0x4D, 0x53, 0x01, 0x01, 0x00}
	if !IsBridgeSatUplink(valid) {
		t.Error("expected true for valid magic")
	}

	invalid := []byte{0x00, 0x00, 0x01, 0x01}
	if IsBridgeSatUplink(invalid) {
		t.Error("expected false for invalid magic")
	}

	short := []byte{0x4D, 0x53}
	if IsBridgeSatUplink(short) {
		t.Error("expected false for short data")
	}

	if IsBridgeSatUplink(nil) {
		t.Error("expected false for nil")
	}
}

func TestDecodeSatUplink_ShortData(t *testing.T) {
	_, _, err := DecodeSatUplink([]byte{0x4D})
	if err != ErrSatTooShort {
		t.Errorf("expected ErrSatTooShort, got %v", err)
	}
}

func TestDecodeSatUplink_BadMagic(t *testing.T) {
	_, _, err := DecodeSatUplink([]byte{0xFF, 0xFF, 0x01, 0x01})
	if err != ErrSatBadMagic {
		t.Errorf("expected ErrSatBadMagic, got %v", err)
	}
}

func TestDecodeSatUplink_BadVersion(t *testing.T) {
	_, _, err := DecodeSatUplink([]byte{0x4D, 0x53, 0xFF, 0x01})
	if err != ErrSatBadVersion {
		t.Errorf("expected ErrSatBadVersion, got %v", err)
	}
}
