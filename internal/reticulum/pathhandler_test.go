package reticulum

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"
)

// pathMockInterface is a test double for Interface.
type pathMockInterface struct {
	name      InterfaceType
	available bool
	sent      [][]byte
	mu        sync.Mutex
}

func (m *pathMockInterface) Name() InterfaceType { return m.name }
func (m *pathMockInterface) Cost() float64       { return 0 }
func (m *pathMockInterface) MTU() int            { return MTU }
func (m *pathMockInterface) IsAvailable() bool   { return m.available }
func (m *pathMockInterface) Send(_ context.Context, _ string, packet []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(packet))
	copy(cp, packet)
	m.sent = append(m.sent, cp)
	return nil
}
func (m *pathMockInterface) sentPackets() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sent
}

func TestPathHandlerRespondsToKnownDest(t *testing.T) {
	router := NewRouter(DefaultRouteTTL)

	// Seed a route.
	var destHash [TruncatedHashLen]byte
	copy(destHash[:], bytes.Repeat([]byte{0xAA}, TruncatedHashLen))
	router.mu.Lock()
	router.routes[destHash] = &RouteEntry{
		DestHash:  destHash,
		Interface: IfaceMQTT,
		Cost:      0,
		Hops:      2,
		LastSeen:  time.Now(),
		ExpiresAt: time.Now().Add(30 * time.Minute),
		AppData:   []byte("test-app"),
	}
	router.mu.Unlock()

	// Set up relay with a mock interface.
	mock := &pathMockInterface{name: IfaceMQTT, available: true}
	relay := NewRelay(router, DefaultRelayConfig())
	relay.RegisterInterface(mock)

	cfg := DefaultPathHandlerConfig()
	cfg.ResponseDelay = 0 // no delay for testing
	ph := NewPathHandler(router, relay, cfg)

	// Build a path request packet.
	req := &PathRequest{DestHash: destHash}
	copy(req.Tag[:], bytes.Repeat([]byte{0x11}, TruncatedHashLen))
	pkt := BuildPathRequestPacket(destHash, req)

	// Handle it.
	handled := ph.HandlePacket(context.Background(), IfaceMQTT, pkt)
	if !handled {
		t.Fatal("expected packet to be handled")
	}

	// Verify response was sent.
	sent := mock.sentPackets()
	if len(sent) != 1 {
		t.Fatalf("expected 1 response, got %d", len(sent))
	}

	// Parse the response packet.
	hdr, err := UnmarshalHeader(sent[0])
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Context != ContextPathResponse {
		t.Errorf("context: got %02x, want %02x", hdr.Context, ContextPathResponse)
	}

	resp, err := UnmarshalPathResponse(hdr.Data)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Hops != 2 {
		t.Errorf("hops: got %d, want 2", resp.Hops)
	}
	if resp.InterfaceType != "mqtt" {
		t.Errorf("iface: got %q, want mqtt", resp.InterfaceType)
	}

	// Check stats.
	stats := ph.Stats()
	if stats.RequestsReceived != 1 {
		t.Errorf("requests_received: got %d, want 1", stats.RequestsReceived)
	}
	if stats.ResponsesSent != 1 {
		t.Errorf("responses_sent: got %d, want 1", stats.ResponsesSent)
	}
}

func TestPathHandlerNoRouteKnown(t *testing.T) {
	router := NewRouter(DefaultRouteTTL)
	relay := NewRelay(router, DefaultRelayConfig())

	cfg := DefaultPathHandlerConfig()
	cfg.ResponseDelay = 0
	ph := NewPathHandler(router, relay, cfg)

	var destHash [TruncatedHashLen]byte
	copy(destHash[:], bytes.Repeat([]byte{0xBB}, TruncatedHashLen))
	req := &PathRequest{DestHash: destHash}
	copy(req.Tag[:], bytes.Repeat([]byte{0x22}, TruncatedHashLen))
	pkt := BuildPathRequestPacket(destHash, req)

	handled := ph.HandlePacket(context.Background(), IfaceMQTT, pkt)
	if !handled {
		t.Fatal("path request should be consumed even with no route")
	}

	stats := ph.Stats()
	if stats.NoRoute != 1 {
		t.Errorf("no_route: got %d, want 1", stats.NoRoute)
	}
}

func TestPathHandlerDedup(t *testing.T) {
	router := NewRouter(DefaultRouteTTL)

	var destHash [TruncatedHashLen]byte
	copy(destHash[:], bytes.Repeat([]byte{0xCC}, TruncatedHashLen))
	router.mu.Lock()
	router.routes[destHash] = &RouteEntry{
		DestHash:  destHash,
		Interface: IfaceMQTT,
		Hops:      1,
		LastSeen:  time.Now(),
		ExpiresAt: time.Now().Add(30 * time.Minute),
	}
	router.mu.Unlock()

	mock := &pathMockInterface{name: IfaceMQTT, available: true}
	relay := NewRelay(router, DefaultRelayConfig())
	relay.RegisterInterface(mock)

	cfg := DefaultPathHandlerConfig()
	cfg.ResponseDelay = 0
	ph := NewPathHandler(router, relay, cfg)

	req := &PathRequest{DestHash: destHash}
	copy(req.Tag[:], bytes.Repeat([]byte{0x33}, TruncatedHashLen))
	pkt := BuildPathRequestPacket(destHash, req)

	// First request — should respond.
	ph.HandlePacket(context.Background(), IfaceMQTT, pkt)
	// Second request with same tag — should be deduped.
	ph.HandlePacket(context.Background(), IfaceMQTT, pkt)

	stats := ph.Stats()
	if stats.ResponsesSent != 1 {
		t.Errorf("responses_sent: got %d, want 1 (dedup should prevent second)", stats.ResponsesSent)
	}
	if stats.Deduplicated != 1 {
		t.Errorf("deduplicated: got %d, want 1", stats.Deduplicated)
	}
}

func TestPathHandlerIgnoresNonPathPackets(t *testing.T) {
	router := NewRouter(DefaultRouteTTL)
	relay := NewRelay(router, DefaultRelayConfig())
	ph := NewPathHandler(router, relay, DefaultPathHandlerConfig())

	// Build a regular data packet (not a path request).
	hdr := Header{
		HeaderType: HeaderType1,
		PacketType: PacketData,
		DestType:   DestSingle,
		Context:    ContextNone,
		Data:       []byte("hello"),
	}
	pkt := hdr.Marshal()

	handled := ph.HandlePacket(context.Background(), IfaceMQTT, pkt)
	if handled {
		t.Fatal("non-path packet should not be handled")
	}
}

func TestPathHandlerPruneStale(t *testing.T) {
	router := NewRouter(DefaultRouteTTL)
	relay := NewRelay(router, DefaultRelayConfig())
	ph := NewPathHandler(router, relay, DefaultPathHandlerConfig())

	// Insert a stale entry.
	var tag [TruncatedHashLen]byte
	copy(tag[:], bytes.Repeat([]byte{0xFF}, TruncatedHashLen))
	ph.mu.Lock()
	ph.seenTags[tag] = time.Now().Add(-1 * time.Minute) // 60s old
	ph.mu.Unlock()

	ph.PruneStale()

	ph.mu.Lock()
	remaining := len(ph.seenTags)
	ph.mu.Unlock()

	if remaining != 0 {
		t.Errorf("expected 0 stale entries after prune, got %d", remaining)
	}
}
