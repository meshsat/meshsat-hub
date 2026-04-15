package reticulum

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
)

// --- Mock satellite sender ---

type mockSatSender struct {
	sent      atomic.Int32
	available bool
	maxPL     int
	cost      float64
	lastDest  string
	lastData  []byte
	sendErr   error
}

func (m *mockSatSender) Send(_ context.Context, deviceID string, payload []byte) error {
	m.sent.Add(1)
	m.lastDest = deviceID
	m.lastData = payload
	return m.sendErr
}
func (m *mockSatSender) IsAvailable(_ context.Context) bool { return m.available }
func (m *mockSatSender) MaxPayload() int                    { return m.maxPL }
func (m *mockSatSender) CostPerMessage() float64            { return m.cost }

// --- Mock MQTT publisher ---

type mockMQTT struct {
	published   atomic.Int32
	lastTopic   string
	lastPayload []byte
	connected   bool
	subHandler  func(string, []byte)
}

func (m *mockMQTT) Publish(topic string, _ byte, _ bool, payload []byte) error {
	m.published.Add(1)
	m.lastTopic = topic
	m.lastPayload = payload
	return nil
}
func (m *mockMQTT) Subscribe(topic string, _ byte, handler func(string, []byte)) error {
	m.subHandler = handler
	return nil
}
func (m *mockMQTT) IsConnected() bool { return m.connected }

// --- Iridium interface tests ---

func TestIridiumInterface_Name(t *testing.T) {
	iface := NewIridiumInterface(nil)
	if iface.Name() != IfaceIridium {
		t.Errorf("name: got %s, want %s", iface.Name(), IfaceIridium)
	}
}

func TestIridiumInterface_Send(t *testing.T) {
	sender := &mockSatSender{available: true, maxPL: 270, cost: 0.05}
	iface := NewIridiumInterface(sender)

	err := iface.Send(context.Background(), "300234065123456", []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if sender.sent.Load() != 1 {
		t.Error("expected 1 send call")
	}
	if sender.lastDest != "300234065123456" {
		t.Errorf("dest: got %s", sender.lastDest)
	}
}

func TestIridiumInterface_SendExceedsMTU(t *testing.T) {
	sender := &mockSatSender{available: true, maxPL: 270, cost: 0.05}
	iface := NewIridiumInterface(sender)

	big := make([]byte, 300)
	err := iface.Send(context.Background(), "imei", big)
	if err == nil {
		t.Error("expected MTU error")
	}
}

func TestIridiumInterface_SendNoSender(t *testing.T) {
	iface := NewIridiumInterface(nil)
	err := iface.Send(context.Background(), "imei", []byte("x"))
	if err == nil {
		t.Error("expected error with nil sender")
	}
}

func TestIridiumInterface_SendError(t *testing.T) {
	sender := &mockSatSender{available: true, maxPL: 270, cost: 0.05, sendErr: fmt.Errorf("satellite error")}
	iface := NewIridiumInterface(sender)

	err := iface.Send(context.Background(), "imei", []byte("x"))
	if err == nil {
		t.Error("expected send error to propagate")
	}
}

func TestIridiumInterface_Cost(t *testing.T) {
	sender := &mockSatSender{cost: 0.05}
	iface := NewIridiumInterface(sender)
	if iface.Cost() != 0.05 {
		t.Errorf("cost: got %f, want 0.05", iface.Cost())
	}
}

func TestIridiumInterface_OnReceive(t *testing.T) {
	iface := NewIridiumInterface(nil)
	var received []byte
	iface.SetHandler(func(ifType InterfaceType, raw []byte) {
		received = raw
		if ifType != IfaceIridium {
			t.Errorf("interface type: got %s, want %s", ifType, IfaceIridium)
		}
	})

	iface.OnReceive([]byte("test-packet"))
	if string(received) != "test-packet" {
		t.Errorf("received: got %q", received)
	}
}

func TestIridiumInterface_OnReceive_NoHandler(t *testing.T) {
	iface := NewIridiumInterface(nil)
	// Should not panic with no handler
	iface.OnReceive([]byte("test"))
}

func TestIridiumInterface_Availability(t *testing.T) {
	iface := NewIridiumInterface(nil)
	if iface.IsAvailable() {
		t.Error("should not be available with nil sender")
	}

	sender := &mockSatSender{available: true, maxPL: 270}
	iface2 := NewIridiumInterface(sender)
	if !iface2.IsAvailable() {
		t.Error("should be available")
	}

	iface2.SetAvailable(false)
	if iface2.IsAvailable() {
		t.Error("should not be available after SetAvailable(false)")
	}
}

// --- MQTT interface tests ---

func TestMQTTInterface_Name(t *testing.T) {
	iface := NewMQTTInterface(nil)
	if iface.Name() != IfaceMQTT {
		t.Errorf("name: got %s, want %s", iface.Name(), IfaceMQTT)
	}
}

func TestMQTTInterface_Cost(t *testing.T) {
	iface := NewMQTTInterface(nil)
	if iface.Cost() != 0 {
		t.Errorf("cost: got %f, want 0", iface.Cost())
	}
}

func TestMQTTInterface_Send(t *testing.T) {
	mqtt := &mockMQTT{connected: true}
	iface := NewMQTTInterface(mqtt)

	err := iface.Send(context.Background(), "", []byte("packet-data"))
	if err != nil {
		t.Fatal(err)
	}
	if mqtt.published.Load() != 1 {
		t.Error("expected 1 publish")
	}
	if mqtt.lastTopic != ReticulumMQTTTopic {
		t.Errorf("topic: got %s, want %s", mqtt.lastTopic, ReticulumMQTTTopic)
	}
}

func TestMQTTInterface_IsAvailable(t *testing.T) {
	mqtt := &mockMQTT{connected: false}
	iface := NewMQTTInterface(mqtt)
	if iface.IsAvailable() {
		t.Error("should not be available when disconnected")
	}

	mqtt.connected = true
	if !iface.IsAvailable() {
		t.Error("should be available when connected")
	}
}

func TestMQTTInterface_Start_Receive(t *testing.T) {
	mqtt := &mockMQTT{connected: true}
	iface := NewMQTTInterface(mqtt)

	var received []byte
	iface.SetHandler(func(ifType InterfaceType, raw []byte) {
		received = raw
	})

	if err := iface.Start(); err != nil {
		t.Fatal(err)
	}

	// Simulate inbound MQTT message
	if mqtt.subHandler != nil {
		mqtt.subHandler(ReticulumMQTTTopic, []byte("inbound-packet"))
	}

	if string(received) != "inbound-packet" {
		t.Errorf("received: got %q", received)
	}
}

// --- Tor interface tests ---

func TestTorInterface_Name(t *testing.T) {
	iface := NewTorInterface("abc123.onion", nil)
	if iface.Name() != IfaceTor {
		t.Errorf("name: got %s, want %s", iface.Name(), IfaceTor)
	}
}

func TestTorInterface_Cost(t *testing.T) {
	iface := NewTorInterface("", nil)
	if iface.Cost() != 0 {
		t.Error("Tor should be free")
	}
}

func TestTorInterface_IsAvailable(t *testing.T) {
	iface := NewTorInterface("", nil)
	if iface.IsAvailable() {
		t.Error("empty onion should not be available")
	}

	iface2 := NewTorInterface("abc.onion", nil)
	if !iface2.IsAvailable() {
		t.Error("should be available with onion address")
	}
}

func TestTorInterface_OnReceive(t *testing.T) {
	iface := NewTorInterface("test.onion", nil)
	var received []byte
	iface.SetHandler(func(_ InterfaceType, raw []byte) {
		received = raw
	})
	iface.OnReceive([]byte("tor-packet"))
	if string(received) != "tor-packet" {
		t.Errorf("received: got %q", received)
	}
}

func TestTorInterface_Send(t *testing.T) {
	mqtt := &mockMQTT{connected: true}
	iface := NewTorInterface("test.onion", mqtt)
	err := iface.Send(context.Background(), "abc123", []byte("pkt"))
	if err != nil {
		t.Fatal(err)
	}
	if mqtt.lastTopic != ReticulumTorTopicPrefix+"abc123" {
		t.Errorf("topic: got %s", mqtt.lastTopic)
	}
}

func TestTorInterface_SendNoMQTT(t *testing.T) {
	iface := NewTorInterface("test.onion", nil)
	err := iface.Send(context.Background(), "abc", []byte("x"))
	if err == nil {
		t.Error("expected error with no mqtt")
	}
}

// --- WireGuard interface tests ---

func TestWireGuardInterface_Name(t *testing.T) {
	iface := NewWireGuardInterface(true, nil)
	if iface.Name() != IfaceWireGuard {
		t.Errorf("name: got %s, want %s", iface.Name(), IfaceWireGuard)
	}
}

func TestWireGuardInterface_Cost(t *testing.T) {
	iface := NewWireGuardInterface(true, nil)
	if iface.Cost() != 0 {
		t.Error("WireGuard should be free")
	}
}

func TestWireGuardInterface_IsAvailable(t *testing.T) {
	iface := NewWireGuardInterface(false, nil)
	if iface.IsAvailable() {
		t.Error("should not be available")
	}
	iface.SetAvailable(true)
	if !iface.IsAvailable() {
		t.Error("should be available after SetAvailable(true)")
	}
}

func TestWireGuardInterface_Send(t *testing.T) {
	mqtt := &mockMQTT{connected: true}
	iface := NewWireGuardInterface(true, mqtt)
	err := iface.Send(context.Background(), "dest123", []byte("pkt"))
	if err != nil {
		t.Fatal(err)
	}
	if mqtt.lastTopic != ReticulumWGTopicPrefix+"dest123" {
		t.Errorf("topic: got %s", mqtt.lastTopic)
	}
}

func TestWireGuardInterface_SendNoMQTT(t *testing.T) {
	iface := NewWireGuardInterface(true, nil)
	err := iface.Send(context.Background(), "dest", []byte("x"))
	if err == nil {
		t.Error("expected error with no mqtt")
	}
}

func TestWireGuardInterface_OnReceive(t *testing.T) {
	iface := NewWireGuardInterface(true, nil)
	var received []byte
	iface.SetHandler(func(_ InterfaceType, raw []byte) {
		received = raw
	})
	iface.OnReceive([]byte("wg-packet"))
	if string(received) != "wg-packet" {
		t.Errorf("received: got %q", received)
	}
}

// --- Globalstar interface tests ---

func TestGlobalstarInterface_Name(t *testing.T) {
	iface := NewGlobalstarInterface(nil)
	if iface.Name() != IfaceGlobalstar {
		t.Errorf("name: got %s, want %s", iface.Name(), IfaceGlobalstar)
	}
}

func TestGlobalstarInterface_MTU(t *testing.T) {
	iface := NewGlobalstarInterface(nil)
	if iface.MTU() != 128 {
		t.Errorf("MTU: got %d, want 128", iface.MTU())
	}
}

func TestGlobalstarInterface_Send(t *testing.T) {
	sender := &mockSatSender{available: true, maxPL: 128, cost: 0.02}
	iface := NewGlobalstarInterface(sender)

	err := iface.Send(context.Background(), "device-2", make([]byte, 100))
	if err != nil {
		t.Fatal(err)
	}
}

func TestGlobalstarInterface_MTUExceeded(t *testing.T) {
	sender := &mockSatSender{available: true, maxPL: 128, cost: 0.02}
	iface := NewGlobalstarInterface(sender)

	err := iface.Send(context.Background(), "dev", make([]byte, 200))
	if err == nil {
		t.Error("expected MTU error")
	}
}

func TestGlobalstarInterface_OnReceive(t *testing.T) {
	iface := NewGlobalstarInterface(nil)
	var received []byte
	iface.SetHandler(func(_ InterfaceType, raw []byte) { received = raw })
	iface.OnReceive([]byte("gs-pkt"))
	if string(received) != "gs-pkt" {
		t.Errorf("received: got %q", received)
	}
}

// --- Interface contract tests ---

func TestAllInterfaces_ImplementInterface(t *testing.T) {
	// Compile-time check that all implementations satisfy Interface.
	var _ Interface = (*IridiumInterface)(nil)
	var _ Interface = (*MQTTInterface)(nil)
	var _ Interface = (*TorInterface)(nil)
	var _ Interface = (*WireGuardInterface)(nil)
	var _ Interface = (*GlobalstarInterface)(nil)
}
