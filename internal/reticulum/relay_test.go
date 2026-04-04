package reticulum

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// mockInterface implements Interface for relay testing.
type mockInterface struct {
	name      InterfaceType
	cost      float64
	mtu       int
	available bool
	sent      int
	lastDest  string
	lastData  []byte
	sendErr   error
}

func (m *mockInterface) Name() InterfaceType { return m.name }
func (m *mockInterface) Cost() float64       { return m.cost }
func (m *mockInterface) MTU() int            { return m.mtu }
func (m *mockInterface) IsAvailable() bool   { return m.available }
func (m *mockInterface) Send(_ context.Context, destID string, packet []byte) error {
	m.sent++
	m.lastDest = destID
	m.lastData = make([]byte, len(packet))
	copy(m.lastData, packet)
	return m.sendErr
}

func setupRelay(t *testing.T) (*Relay, *Announce, *mockInterface) {
	t.Helper()
	router := NewRouter(5 * time.Minute)
	cfg := DefaultRelayConfig()
	relay := NewRelay(router, cfg)

	// Create an announce and learn a route via MQTT.
	a := makeTestAnnounce(t, "meshsat.hub")
	router.ProcessAnnounce(a, IfaceMQTT)

	// Register a mock MQTT interface.
	mqttIface := &mockInterface{
		name:      IfaceMQTT,
		cost:      0,
		mtu:       500,
		available: true,
	}
	relay.RegisterInterface(mqttIface)

	return relay, a, mqttIface
}

func TestRelay_Forward_Success(t *testing.T) {
	relay, a, mqttIface := setupRelay(t)

	// Build a data packet destined for the announce's dest hash.
	hdr := &Header{
		HeaderType: HeaderType1,
		PacketType: PacketData,
		DestHash:   a.DestHash,
		Context:    ContextNone,
		Hops:       0,
		Data:       []byte("payload"),
	}
	raw := hdr.Marshal()

	// Forward from Iridium → should go to MQTT (where the route points).
	err := relay.Forward(context.Background(), IfaceIridium, raw)
	if err != nil {
		t.Fatalf("forward failed: %v", err)
	}

	if mqttIface.sent != 1 {
		t.Errorf("expected 1 send, got %d", mqttIface.sent)
	}

	stats := relay.Stats()
	if stats.Forwarded != 1 {
		t.Errorf("forwarded: got %d, want 1", stats.Forwarded)
	}
}

func TestRelay_Forward_NoRoute(t *testing.T) {
	router := NewRouter(5 * time.Minute)
	relay := NewRelay(router, DefaultRelayConfig())

	hdr := &Header{
		HeaderType: HeaderType1,
		PacketType: PacketData,
		Context:    ContextNone,
		Hops:       0,
		Data:       []byte("x"),
	}
	// Unknown destination.
	hdr.DestHash[0] = 0xFF
	raw := hdr.Marshal()

	err := relay.Forward(context.Background(), IfaceIridium, raw)
	if err == nil {
		t.Error("expected no-route error")
	}

	stats := relay.Stats()
	if stats.NoRoute != 1 {
		t.Errorf("no_route: got %d, want 1", stats.NoRoute)
	}
}

func TestRelay_Forward_LoopPrevention(t *testing.T) {
	relay, a, _ := setupRelay(t)

	hdr := &Header{
		HeaderType: HeaderType1,
		PacketType: PacketData,
		DestHash:   a.DestHash,
		Context:    ContextNone,
		Hops:       0,
		Data:       []byte("x"),
	}
	raw := hdr.Marshal()

	// Forward from MQTT → route says MQTT → should be rejected (loop).
	err := relay.Forward(context.Background(), IfaceMQTT, raw)
	if err == nil {
		t.Error("expected loop prevention error")
	}
}

func TestRelay_Forward_MaxHops(t *testing.T) {
	relay, a, _ := setupRelay(t)

	hdr := &Header{
		HeaderType: HeaderType1,
		PacketType: PacketData,
		DestHash:   a.DestHash,
		Context:    ContextNone,
		Hops:       byte(PathfinderM), // Already at max.
		Data:       []byte("x"),
	}
	raw := hdr.Marshal()

	err := relay.Forward(context.Background(), IfaceIridium, raw)
	if err == nil {
		t.Error("expected max hops error")
	}

	stats := relay.Stats()
	if stats.Dropped != 1 {
		t.Errorf("dropped: got %d, want 1", stats.Dropped)
	}
}

func TestRelay_Forward_InterfaceUnavailable(t *testing.T) {
	relay, a, mqttIface := setupRelay(t)
	mqttIface.available = false

	hdr := &Header{
		HeaderType: HeaderType1,
		PacketType: PacketData,
		DestHash:   a.DestHash,
		Context:    ContextNone,
		Hops:       0,
		Data:       []byte("x"),
	}
	raw := hdr.Marshal()

	err := relay.Forward(context.Background(), IfaceIridium, raw)
	if err == nil {
		t.Error("expected unavailable interface error")
	}
}

func TestRelay_Forward_SendError(t *testing.T) {
	relay, a, mqttIface := setupRelay(t)
	mqttIface.sendErr = fmt.Errorf("send failed")

	hdr := &Header{
		HeaderType: HeaderType1,
		PacketType: PacketData,
		DestHash:   a.DestHash,
		Context:    ContextNone,
		Hops:       0,
		Data:       []byte("x"),
	}
	raw := hdr.Marshal()

	err := relay.Forward(context.Background(), IfaceIridium, raw)
	if err == nil {
		t.Error("expected send error")
	}

	stats := relay.Stats()
	if stats.Dropped != 1 {
		t.Errorf("dropped: got %d, want 1", stats.Dropped)
	}
}

func TestRelay_Forward_MTUExceeded(t *testing.T) {
	relay, a, mqttIface := setupRelay(t)
	mqttIface.mtu = 20 // Very small MTU.

	hdr := &Header{
		HeaderType: HeaderType1,
		PacketType: PacketData,
		DestHash:   a.DestHash,
		Context:    ContextNone,
		Hops:       0,
		Data:       make([]byte, 100), // Will exceed 20-byte MTU.
	}
	raw := hdr.Marshal()

	err := relay.Forward(context.Background(), IfaceIridium, raw)
	if err == nil {
		t.Error("expected MTU exceeded error")
	}
}

func TestRelay_Forward_HopIncremented(t *testing.T) {
	relay, a, mqttIface := setupRelay(t)

	hdr := &Header{
		HeaderType: HeaderType1,
		PacketType: PacketData,
		DestHash:   a.DestHash,
		Context:    ContextNone,
		Hops:       5,
		Data:       []byte("x"),
	}
	raw := hdr.Marshal()

	err := relay.Forward(context.Background(), IfaceIridium, raw)
	if err != nil {
		t.Fatal(err)
	}

	// Parse the forwarded packet — hop count should be 6.
	fwd, err := UnmarshalHeader(mqttIface.lastData)
	if err != nil {
		t.Fatal(err)
	}
	if fwd.Hops != 6 {
		t.Errorf("hops: got %d, want 6", fwd.Hops)
	}
}

func TestRelay_Forward_InvalidPacket(t *testing.T) {
	relay, _, _ := setupRelay(t)

	err := relay.Forward(context.Background(), IfaceIridium, []byte{0x00})
	if err == nil {
		t.Error("expected error for invalid packet")
	}
}

func TestRelay_Stats(t *testing.T) {
	router := NewRouter(5 * time.Minute)
	relay := NewRelay(router, DefaultRelayConfig())

	stats := relay.Stats()
	if stats.Forwarded != 0 || stats.Dropped != 0 || stats.NoRoute != 0 || stats.RateLimit != 0 {
		t.Error("initial stats should all be zero")
	}
}

func TestRelay_RegisterInterface(t *testing.T) {
	router := NewRouter(5 * time.Minute)
	relay := NewRelay(router, DefaultRelayConfig())

	iface := &mockInterface{name: IfaceIridium, available: true, mtu: 270}
	relay.RegisterInterface(iface)

	// No panic, interface stored — verified indirectly by Forward using it.
}

func TestRelay_RateLimiting(t *testing.T) {
	router := NewRouter(5 * time.Minute)
	cfg := RelayConfig{MaxPacketsPerSec: 2, RequireCreditsForPaid: false}
	relay := NewRelay(router, cfg)

	a := makeTestAnnounce(t, "meshsat.hub")
	router.ProcessAnnounce(a, IfaceMQTT)

	mqttIface := &mockInterface{name: IfaceMQTT, cost: 0, mtu: 500, available: true}
	relay.RegisterInterface(mqttIface)

	hdr := &Header{
		HeaderType: HeaderType1,
		PacketType: PacketData,
		DestHash:   a.DestHash,
		Context:    ContextNone,
		Data:       []byte("x"),
	}

	// Send 2 packets (should succeed — burst of 2).
	for range 2 {
		raw := hdr.Marshal()
		hdr.Hops = 0 // Reset for each send.
		if err := relay.Forward(context.Background(), IfaceIridium, raw); err != nil {
			t.Fatalf("expected success, got: %v", err)
		}
	}

	// Third packet should be rate-limited.
	raw := hdr.Marshal()
	err := relay.Forward(context.Background(), IfaceIridium, raw)
	if err == nil {
		t.Error("expected rate limit error")
	}

	stats := relay.Stats()
	if stats.RateLimit < 1 {
		t.Errorf("rate_limit: got %d, want >= 1", stats.RateLimit)
	}
}

func TestRelay_SendVia_Success(t *testing.T) {
	router := NewRouter(5 * time.Minute)
	relay := NewRelay(router, DefaultRelayConfig())

	mqttIface := &mockInterface{name: IfaceMQTT, mtu: 500, available: true}
	relay.RegisterInterface(mqttIface)

	data := []byte("custody-ack-payload")
	if err := relay.SendVia(context.Background(), IfaceMQTT, data); err != nil {
		t.Fatalf("SendVia: %v", err)
	}
	if mqttIface.sent != 1 {
		t.Fatalf("expected 1 send, got %d", mqttIface.sent)
	}
}

func TestRelay_SendVia_NotRegistered(t *testing.T) {
	router := NewRouter(5 * time.Minute)
	relay := NewRelay(router, DefaultRelayConfig())

	err := relay.SendVia(context.Background(), IfaceMQTT, []byte("data"))
	if err == nil {
		t.Fatal("expected error for unregistered interface")
	}
}

func TestRelay_SendVia_Unavailable(t *testing.T) {
	router := NewRouter(5 * time.Minute)
	relay := NewRelay(router, DefaultRelayConfig())

	mqttIface := &mockInterface{name: IfaceMQTT, mtu: 500, available: false}
	relay.RegisterInterface(mqttIface)

	err := relay.SendVia(context.Background(), IfaceMQTT, []byte("data"))
	if err == nil {
		t.Fatal("expected error for unavailable interface")
	}
}

func TestRelay_SendVia_MTUExceeded(t *testing.T) {
	router := NewRouter(5 * time.Minute)
	relay := NewRelay(router, DefaultRelayConfig())

	mqttIface := &mockInterface{name: IfaceMQTT, mtu: 10, available: true}
	relay.RegisterInterface(mqttIface)

	err := relay.SendVia(context.Background(), IfaceMQTT, make([]byte, 20))
	if err == nil {
		t.Fatal("expected error for MTU exceeded")
	}
}
