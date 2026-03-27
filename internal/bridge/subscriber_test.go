package bridge

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/bus"
	"github.com/cubeos-app/meshsat-hub/internal/protocol"
	"github.com/cubeos-app/meshsat-hub/internal/store"
)

// --- Mock store ---

type mockStore struct {
	store.Store // embed to satisfy interface; unimplemented methods will panic if called

	bridges           map[string]*store.Bridge
	devices           map[string]*store.Device
	bridgeOnline      map[string]bool
	bridgeHealth      map[string]string
	bridgeLastSeen    map[string]bool
	deviceBridgeMap   map[string]string // imei -> bridge_id
	createDeviceCalls int
}

func newMockStore() *mockStore {
	return &mockStore{
		bridges:         make(map[string]*store.Bridge),
		devices:         make(map[string]*store.Device),
		bridgeOnline:    make(map[string]bool),
		bridgeHealth:    make(map[string]string),
		bridgeLastSeen:  make(map[string]bool),
		deviceBridgeMap: make(map[string]string),
	}
}

func (m *mockStore) CreateOrUpdateBridge(_ context.Context, _ string, b *store.Bridge) error {
	m.bridges[b.BridgeID] = b
	return nil
}

func (m *mockStore) SetBridgeOnline(_ context.Context, _ string, bridgeID string, online bool) error {
	m.bridgeOnline[bridgeID] = online
	return nil
}

func (m *mockStore) SetBridgeHealth(_ context.Context, _ string, bridgeID string, health string) error {
	m.bridgeHealth[bridgeID] = health
	return nil
}

func (m *mockStore) TouchBridgeLastSeen(_ context.Context, _ string, bridgeID string) error {
	m.bridgeLastSeen[bridgeID] = true
	return nil
}

func (m *mockStore) MarkStaleBridgesOffline(_ context.Context, _ time.Duration) (int64, error) {
	return 0, nil
}

func (m *mockStore) AssociateDeviceWithBridge(_ context.Context, _ string, imei string, bridgeID string) error {
	m.deviceBridgeMap[imei] = bridgeID
	return nil
}

func (m *mockStore) GetDevice(_ context.Context, _ string, imei string) (*store.Device, error) {
	d, ok := m.devices[imei]
	if !ok {
		return nil, nil
	}
	return d, nil
}

func (m *mockStore) CreateDevice(_ context.Context, _ string, d *store.Device) error {
	m.devices[d.IMEI] = d
	m.createDeviceCalls++
	return nil
}

func (m *mockStore) GetBridge(_ context.Context, _ string, bridgeID string) (*store.Bridge, error) {
	b, ok := m.bridges[bridgeID]
	if !ok {
		return nil, nil
	}
	return b, nil
}

// --- Mock Reticulum router ---

type mockRetRouter struct {
	injected  map[string]string // destHash -> iface
	removed   map[string]bool
	refreshed map[string]int
}

func newMockRetRouter() *mockRetRouter {
	return &mockRetRouter{
		injected:  make(map[string]string),
		removed:   make(map[string]bool),
		refreshed: make(map[string]int),
	}
}

func (m *mockRetRouter) InjectRoute(destHashHex string, _ []byte, iface string, _ int) bool {
	m.injected[destHashHex] = iface
	return true
}

func (m *mockRetRouter) RemoveHex(destHex string) bool {
	m.removed[destHex] = true
	return true
}

func (m *mockRetRouter) RefreshRoute(destHashHex string) bool {
	m.refreshed[destHashHex]++
	return true
}

// --- Mock message bus ---

type mockBus struct {
	subscriptions map[string]bus.MessageHandler
}

func newMockBus() *mockBus {
	return &mockBus{subscriptions: make(map[string]bus.MessageHandler)}
}

func (m *mockBus) Connect() error                            { return nil }
func (m *mockBus) Publish(string, byte, bool, []byte) error  { return nil }
func (m *mockBus) PublishJSON(string, byte, bool, any) error { return nil }
func (m *mockBus) IsConnected() bool                         { return true }
func (m *mockBus) Disconnect()                               {}
func (m *mockBus) QueueSubscribe(t string, q byte, _ string, h bus.MessageHandler) error {
	return m.Subscribe(t, q, h)
}

func (m *mockBus) Subscribe(topic string, _ byte, handler bus.MessageHandler) error {
	m.subscriptions[topic] = handler
	return nil
}

func (m *mockBus) deliver(topic string, payload []byte) {
	for pattern, handler := range m.subscriptions {
		if topicMatchesPattern(pattern, topic) {
			handler(topic, payload)
		}
	}
}

// topicMatchesPattern does basic MQTT wildcard matching for + (single level).
func topicMatchesPattern(pattern, topic string) bool {
	pp := splitTopic(pattern)
	tp := splitTopic(topic)
	if len(pp) != len(tp) {
		return false
	}
	for i, p := range pp {
		if p == "+" {
			continue
		}
		if p != tp[i] {
			return false
		}
	}
	return true
}

func splitTopic(t string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(t); i++ {
		if t[i] == '/' {
			parts = append(parts, t[start:i])
			start = i + 1
		}
	}
	parts = append(parts, t[start:])
	return parts
}

// --- Topic parsing tests ---

func TestExtractBridgeIDFromTopic(t *testing.T) {
	tests := []struct {
		topic string
		want  string
	}{
		{"meshsat/bridge/mule01/birth", "mule01"},
		{"meshsat/bridge/mule01/death", "mule01"},
		{"meshsat/bridge/mule01/health", "mule01"},
		{"meshsat/bridge/mule01/device/300234/birth", "mule01"},
		{"meshsat/bridge/mule01/device/300234/death", "mule01"},
		{"meshsat/bridge/pi-field-01/birth", "pi-field-01"},
		{"meshsat/other/topic", ""},
		{"bad", ""},
		{"", ""},
	}

	for _, tt := range tests {
		got := extractBridgeIDFromTopic(tt.topic)
		if got != tt.want {
			t.Errorf("extractBridgeIDFromTopic(%q) = %q, want %q", tt.topic, got, tt.want)
		}
	}
}

func TestExtractDeviceIDFromTopic(t *testing.T) {
	tests := []struct {
		topic string
		want  string
	}{
		{"meshsat/bridge/mule01/device/300234/birth", "300234"},
		{"meshsat/bridge/mule01/device/300234/death", "300234"},
		{"meshsat/bridge/mule01/device/imt-9704/birth", "imt-9704"},
		{"meshsat/bridge/mule01/birth", ""},
		{"meshsat/bridge/mule01/health", ""},
		{"bad", ""},
	}

	for _, tt := range tests {
		got := extractDeviceIDFromTopic(tt.topic)
		if got != tt.want {
			t.Errorf("extractDeviceIDFromTopic(%q) = %q, want %q", tt.topic, got, tt.want)
		}
	}
}

// --- Handler tests ---

func TestHandleBridgeBirth(t *testing.T) {
	ms := newMockStore()
	mb := newMockBus()
	sub := NewSubscriber(mb, ms, "default")
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}

	birth := protocol.BridgeBirth{
		Protocol: protocol.ProtocolVersion,
		BridgeID: "mule01",
		Version:  "0.2.0",
		Hostname: "mule01.local",
		Mode:     "direct",
		TenantID: "default",
		Location: &protocol.Location{Lat: 52.5, Lon: 13.4, Alt: 35},
		Interfaces: []protocol.InterfaceInfo{
			{Name: "iridium_0", Type: "iridium_imt", Status: "online", IMEI: "300258060902280"},
			{Name: "mesh_0", Type: "meshtastic", Status: "online"},
		},
		Capabilities: []string{"sbd", "imt", "meshtastic"},
		Reticulum:    &protocol.ReticulumInfo{IdentityHash: "abc123", PublicKey: "pubkey123", TransportEnabled: true},
		CoTType:      protocol.CoTBridge,
		CoTCallsign:  "MULE01",
		UptimeSec:    3600,
		Timestamp:    time.Now(),
	}
	payload, _ := json.Marshal(birth)
	mb.deliver("meshsat/bridge/mule01/birth", payload)

	// Verify bridge was created.
	b, ok := ms.bridges["mule01"]
	if !ok {
		t.Fatal("bridge not created in store")
	}
	if b.Hostname != "mule01.local" {
		t.Errorf("hostname = %q, want %q", b.Hostname, "mule01.local")
	}
	if b.Version != "0.2.0" {
		t.Errorf("version = %q, want %q", b.Version, "0.2.0")
	}
	if b.LocationLat != 52.5 {
		t.Errorf("lat = %f, want 52.5", b.LocationLat)
	}
	if b.ReticulumHash != "abc123" {
		t.Errorf("reticulum_hash = %q, want %q", b.ReticulumHash, "abc123")
	}
	if !b.Online {
		t.Error("bridge should be online")
	}
	if b.LastBirth == "" {
		t.Error("last_birth should be set")
	}

	// Verify bridge marked online.
	if online, ok := ms.bridgeOnline["mule01"]; !ok || !online {
		t.Error("bridge should be set online in store")
	}

	// Verify IMEI device associated.
	if bridgeID, ok := ms.deviceBridgeMap["300258060902280"]; !ok || bridgeID != "mule01" {
		t.Error("IMEI 300258060902280 should be associated with mule01")
	}

	// Non-IMEI interface should not be in device map.
	if _, ok := ms.deviceBridgeMap[""]; ok {
		t.Error("empty IMEI should not be associated")
	}
}

func TestHandleBridgeDeath(t *testing.T) {
	ms := newMockStore()
	mb := newMockBus()
	sub := NewSubscriber(mb, ms, "default")
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}

	death := protocol.BridgeDeath{
		Protocol:  protocol.ProtocolVersion,
		BridgeID:  "mule01",
		Reason:    "shutdown",
		Timestamp: time.Now(),
	}
	payload, _ := json.Marshal(death)
	mb.deliver("meshsat/bridge/mule01/death", payload)

	if online, ok := ms.bridgeOnline["mule01"]; !ok || online {
		t.Error("bridge should be set offline")
	}
}

func TestHandleBridgeHealth(t *testing.T) {
	ms := newMockStore()
	mb := newMockBus()
	sub := NewSubscriber(mb, ms, "default")
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}

	health := protocol.BridgeHealth{
		Protocol:  protocol.ProtocolVersion,
		BridgeID:  "mule01",
		UptimeSec: 7200,
		CPUPct:    12.5,
		MemPct:    45.0,
		DiskPct:   30.0,
		Timestamp: time.Now(),
	}
	payload, _ := json.Marshal(health)
	mb.deliver("meshsat/bridge/mule01/health", payload)

	if h, ok := ms.bridgeHealth["mule01"]; !ok || h == "" {
		t.Error("bridge health should be stored")
	}

	if !ms.bridgeLastSeen["mule01"] {
		t.Error("bridge last_seen should be touched")
	}
}

func TestHandleBridgeHealth_SetsOnline(t *testing.T) {
	ms := newMockStore()
	mb := newMockBus()
	sub := NewSubscriber(mb, ms, "default")
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}

	// Simulate a bridge that the reaper marked offline.
	ms.bridgeOnline["mule01"] = false

	health := protocol.BridgeHealth{
		Protocol:  protocol.ProtocolVersion,
		BridgeID:  "mule01",
		UptimeSec: 7200,
		CPUPct:    12.5,
		MemPct:    45.0,
		DiskPct:   30.0,
		Timestamp: time.Now(),
	}
	payload, _ := json.Marshal(health)
	mb.deliver("meshsat/bridge/mule01/health", payload)

	// Health should re-set the bridge online.
	if online, ok := ms.bridgeOnline["mule01"]; !ok || !online {
		t.Error("health message should set bridge back online")
	}
}

func TestHandleDeviceBirth_AutoProvision(t *testing.T) {
	ms := newMockStore()
	mb := newMockBus()
	sub := NewSubscriber(mb, ms, "default")
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}

	birth := protocol.DeviceBirth{
		Protocol:  protocol.ProtocolVersion,
		DeviceID:  "iridium_0",
		BridgeID:  "mule01",
		Type:      "iridium_imt",
		Label:     "RockBLOCK 9704",
		IMEI:      "300258060902280",
		Timestamp: time.Now(),
	}
	payload, _ := json.Marshal(birth)
	mb.deliver("meshsat/bridge/mule01/device/iridium_0/birth", payload)

	// Device should be auto-provisioned.
	if ms.createDeviceCalls != 1 {
		t.Errorf("createDevice calls = %d, want 1", ms.createDeviceCalls)
	}
	d, ok := ms.devices["300258060902280"]
	if !ok {
		t.Fatal("device not auto-provisioned")
	}
	if d.Type != "iridium_imt" {
		t.Errorf("device type = %q, want %q", d.Type, "iridium_imt")
	}
	if d.Label != "RockBLOCK 9704" {
		t.Errorf("device label = %q, want %q", d.Label, "RockBLOCK 9704")
	}

	// Device should be associated with bridge.
	if bridgeID := ms.deviceBridgeMap["300258060902280"]; bridgeID != "mule01" {
		t.Errorf("device bridge = %q, want %q", bridgeID, "mule01")
	}
}

func TestHandleDeviceBirth_ExistingDevice(t *testing.T) {
	ms := newMockStore()
	ms.devices["300258060902280"] = &store.Device{IMEI: "300258060902280", Label: "Existing"}

	mb := newMockBus()
	sub := NewSubscriber(mb, ms, "default")
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}

	birth := protocol.DeviceBirth{
		Protocol:  protocol.ProtocolVersion,
		DeviceID:  "iridium_0",
		BridgeID:  "mule01",
		Type:      "iridium_imt",
		IMEI:      "300258060902280",
		Timestamp: time.Now(),
	}
	payload, _ := json.Marshal(birth)
	mb.deliver("meshsat/bridge/mule01/device/iridium_0/birth", payload)

	// Should NOT create a new device (already exists).
	if ms.createDeviceCalls != 0 {
		t.Errorf("createDevice calls = %d, want 0 (device already exists)", ms.createDeviceCalls)
	}

	// But should still associate.
	if bridgeID := ms.deviceBridgeMap["300258060902280"]; bridgeID != "mule01" {
		t.Errorf("device bridge = %q, want %q", bridgeID, "mule01")
	}
}

func TestHandleDeviceBirth_NoIMEI(t *testing.T) {
	ms := newMockStore()
	mb := newMockBus()
	sub := NewSubscriber(mb, ms, "default")
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}

	birth := protocol.DeviceBirth{
		Protocol:  protocol.ProtocolVersion,
		DeviceID:  "mesh_0",
		BridgeID:  "mule01",
		Type:      "meshtastic",
		Label:     "LoRa Node",
		Timestamp: time.Now(),
	}
	payload, _ := json.Marshal(birth)
	mb.deliver("meshsat/bridge/mule01/device/mesh_0/birth", payload)

	// No IMEI — should not auto-provision or associate.
	if ms.createDeviceCalls != 0 {
		t.Errorf("createDevice calls = %d, want 0 (no IMEI)", ms.createDeviceCalls)
	}
}

func TestHandleDeviceDeath(t *testing.T) {
	ms := newMockStore()
	mb := newMockBus()
	sub := NewSubscriber(mb, ms, "default")
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}

	death := protocol.DeviceDeath{
		Protocol:  protocol.ProtocolVersion,
		DeviceID:  "iridium_0",
		BridgeID:  "mule01",
		Reason:    "offline",
		Timestamp: time.Now(),
	}
	payload, _ := json.Marshal(death)
	// Should not panic — just logs.
	mb.deliver("meshsat/bridge/mule01/device/iridium_0/death", payload)
}

func TestProtocolVersionValidation(t *testing.T) {
	ms := newMockStore()
	mb := newMockBus()
	sub := NewSubscriber(mb, ms, "default")
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}

	// Birth with wrong protocol version should be rejected.
	birth := protocol.BridgeBirth{
		Protocol:  "meshsat-uplink/v999",
		BridgeID:  "mule01",
		Version:   "0.2.0",
		Timestamp: time.Now(),
	}
	payload, _ := json.Marshal(birth)
	mb.deliver("meshsat/bridge/mule01/birth", payload)

	if _, ok := ms.bridges["mule01"]; ok {
		t.Error("bridge with unknown protocol version should be rejected")
	}

	// Death with wrong version.
	death := protocol.BridgeDeath{
		Protocol:  "meshsat-uplink/v999",
		BridgeID:  "mule01",
		Reason:    "shutdown",
		Timestamp: time.Now(),
	}
	payload, _ = json.Marshal(death)
	mb.deliver("meshsat/bridge/mule01/death", payload)
	if _, ok := ms.bridgeOnline["mule01"]; ok {
		t.Error("death with unknown protocol version should be rejected")
	}
}

func TestStart_SubscribesAllTopics(t *testing.T) {
	mb := newMockBus()
	sub := NewSubscriber(mb, newMockStore(), "default")
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}

	expected := []string{
		protocol.SubBridgeBirth,
		protocol.SubBridgeDeath,
		protocol.SubBridgeHealth,
		protocol.SubDeviceBirth,
		protocol.SubDeviceDeath,
	}

	for _, topic := range expected {
		if _, ok := mb.subscriptions[topic]; !ok {
			t.Errorf("missing subscription for %q", topic)
		}
	}
}

func TestResolveTenantID(t *testing.T) {
	sub := &Subscriber{tenantID: "default"}

	if got := sub.resolveTenantID("custom"); got != "custom" {
		t.Errorf("resolveTenantID(%q) = %q, want %q", "custom", got, "custom")
	}
	if got := sub.resolveTenantID(""); got != "default" {
		t.Errorf("resolveTenantID(%q) = %q, want %q", "", got, "default")
	}
}

func TestHandleBridgeBirth_StaleBirthNotMarkedOnline(t *testing.T) {
	ms := newMockStore()
	mb := newMockBus()
	sub := NewSubscriber(mb, ms, "default")
	sub.SetStaleThreshold(5 * time.Minute)
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}

	// Simulate a retained birth with a timestamp 2 hours ago.
	birth := protocol.BridgeBirth{
		Protocol:     protocol.ProtocolVersion,
		BridgeID:     "stale-bridge",
		Version:      "0.18.0",
		Hostname:     "stale-bridge.local",
		Mode:         "direct",
		TenantID:     "default",
		Interfaces:   []protocol.InterfaceInfo{{Name: "mesh_0", Type: "meshtastic", Status: "online"}},
		Capabilities: []string{"meshtastic"},
		Timestamp:    time.Now().Add(-2 * time.Hour),
	}
	payload, _ := json.Marshal(birth)
	mb.deliver("meshsat/bridge/stale-bridge/birth", payload)

	// Bridge metadata should still be stored.
	b, ok := ms.bridges["stale-bridge"]
	if !ok {
		t.Fatal("bridge metadata not created in store")
	}
	if b.Hostname != "stale-bridge.local" {
		t.Errorf("hostname = %q, want %q", b.Hostname, "stale-bridge.local")
	}

	// But it should NOT be marked online.
	if online, ok := ms.bridgeOnline["stale-bridge"]; ok && online {
		t.Error("stale retained birth should NOT set bridge online")
	}
}

func TestHandleBridgeBirth_FreshBirthMarkedOnline(t *testing.T) {
	ms := newMockStore()
	mb := newMockBus()
	sub := NewSubscriber(mb, ms, "default")
	sub.SetStaleThreshold(5 * time.Minute)
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}

	// Fresh birth — timestamp is now.
	birth := protocol.BridgeBirth{
		Protocol:     protocol.ProtocolVersion,
		BridgeID:     "fresh-bridge",
		Version:      "0.18.0",
		Hostname:     "fresh-bridge.local",
		Mode:         "direct",
		TenantID:     "default",
		Interfaces:   []protocol.InterfaceInfo{{Name: "mesh_0", Type: "meshtastic", Status: "online"}},
		Capabilities: []string{"meshtastic"},
		Timestamp:    time.Now(),
	}
	payload, _ := json.Marshal(birth)
	mb.deliver("meshsat/bridge/fresh-bridge/birth", payload)

	// Should be marked online.
	if online, ok := ms.bridgeOnline["fresh-bridge"]; !ok || !online {
		t.Error("fresh birth should set bridge online")
	}
}

// --- Reticulum router integration tests ---

func TestBridgeBirth_InjectsReticulumRoute(t *testing.T) {
	ms := newMockStore()
	mb := newMockBus()
	rr := newMockRetRouter()
	sub := NewSubscriber(mb, ms, "default")
	sub.SetReticulumRouter(rr)
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}

	birth := protocol.BridgeBirth{
		Protocol:     protocol.ProtocolVersion,
		BridgeID:     "pi-field-01",
		Version:      "0.2.0",
		Hostname:     "pi-field-01.local",
		Mode:         "direct",
		TenantID:     "default",
		Interfaces:   []protocol.InterfaceInfo{{Name: "mesh_0", Type: "meshtastic", Status: "online"}},
		Capabilities: []string{"meshtastic"},
		Reticulum:    &protocol.ReticulumInfo{IdentityHash: "aabbccdd11223344", PublicKey: "deadbeef", TransportEnabled: true},
		Timestamp:    time.Now(),
	}
	payload, _ := json.Marshal(birth)
	mb.deliver("meshsat/bridge/pi-field-01/birth", payload)

	// Verify route was injected.
	if iface, ok := rr.injected["aabbccdd11223344"]; !ok {
		t.Error("expected reticulum route to be injected on birth")
	} else if iface != "mqtt" {
		t.Errorf("injected iface = %q, want %q", iface, "mqtt")
	}
}

func TestBridgeBirth_StaleBirthDoesNotInjectRoute(t *testing.T) {
	ms := newMockStore()
	mb := newMockBus()
	rr := newMockRetRouter()
	sub := NewSubscriber(mb, ms, "default")
	sub.SetReticulumRouter(rr)
	sub.SetStaleThreshold(5 * time.Minute)
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}

	birth := protocol.BridgeBirth{
		Protocol:     protocol.ProtocolVersion,
		BridgeID:     "stale-pi",
		Version:      "0.2.0",
		Hostname:     "stale-pi.local",
		Mode:         "direct",
		TenantID:     "default",
		Capabilities: []string{"meshtastic"},
		Reticulum:    &protocol.ReticulumInfo{IdentityHash: "aabbccdd11223344", PublicKey: "deadbeef", TransportEnabled: true},
		Timestamp:    time.Now().Add(-2 * time.Hour),
	}
	payload, _ := json.Marshal(birth)
	mb.deliver("meshsat/bridge/stale-pi/birth", payload)

	// Stale birth should NOT inject a route.
	if _, ok := rr.injected["aabbccdd11223344"]; ok {
		t.Error("stale retained birth should NOT inject reticulum route")
	}
}

func TestBridgeBirth_NoReticulumInfoDoesNotInjectRoute(t *testing.T) {
	ms := newMockStore()
	mb := newMockBus()
	rr := newMockRetRouter()
	sub := NewSubscriber(mb, ms, "default")
	sub.SetReticulumRouter(rr)
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}

	birth := protocol.BridgeBirth{
		Protocol:     protocol.ProtocolVersion,
		BridgeID:     "no-ret",
		Version:      "0.2.0",
		Hostname:     "no-ret.local",
		Mode:         "direct",
		TenantID:     "default",
		Capabilities: []string{"sbd"},
		Timestamp:    time.Now(),
	}
	payload, _ := json.Marshal(birth)
	mb.deliver("meshsat/bridge/no-ret/birth", payload)

	if len(rr.injected) != 0 {
		t.Error("bridge without reticulum info should not inject route")
	}
}

func TestBridgeDeath_RemovesReticulumRoute(t *testing.T) {
	ms := newMockStore()
	mb := newMockBus()
	rr := newMockRetRouter()
	sub := NewSubscriber(mb, ms, "default")
	sub.SetReticulumRouter(rr)
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}

	// Pre-populate bridge in store with Reticulum hash.
	ms.bridges["pi-field-01"] = &store.Bridge{
		BridgeID:      "pi-field-01",
		ReticulumHash: "aabbccdd11223344",
	}

	death := protocol.BridgeDeath{
		Protocol:  protocol.ProtocolVersion,
		BridgeID:  "pi-field-01",
		Reason:    "shutdown",
		Timestamp: time.Now(),
	}
	payload, _ := json.Marshal(death)
	mb.deliver("meshsat/bridge/pi-field-01/death", payload)

	if !rr.removed["aabbccdd11223344"] {
		t.Error("expected reticulum route to be removed on death")
	}
}

func TestBridgeHealth_RefreshesReticulumRoute(t *testing.T) {
	ms := newMockStore()
	mb := newMockBus()
	rr := newMockRetRouter()
	sub := NewSubscriber(mb, ms, "default")
	sub.SetReticulumRouter(rr)
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}

	// Pre-populate bridge in store with Reticulum hash.
	ms.bridges["pi-field-01"] = &store.Bridge{
		BridgeID:      "pi-field-01",
		ReticulumHash: "aabbccdd11223344",
	}

	health := protocol.BridgeHealth{
		Protocol:  protocol.ProtocolVersion,
		BridgeID:  "pi-field-01",
		UptimeSec: 3600,
		CPUPct:    10,
		Timestamp: time.Now(),
	}
	payload, _ := json.Marshal(health)
	mb.deliver("meshsat/bridge/pi-field-01/health", payload)

	if rr.refreshed["aabbccdd11223344"] != 1 {
		t.Errorf("expected route refresh count = 1, got %d", rr.refreshed["aabbccdd11223344"])
	}
}
