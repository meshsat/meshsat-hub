package reticulum

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// hintMockMQTT captures published messages for test verification.
type hintMockMQTT struct {
	mu        sync.Mutex
	published []mqttMsg
	connected bool
}

type mqttMsg struct {
	topic    string
	qos      byte
	retained bool
	payload  []byte
}

func (m *hintMockMQTT) Publish(topic string, qos byte, retained bool, payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.published = append(m.published, mqttMsg{topic, qos, retained, payload})
	return nil
}

func (m *hintMockMQTT) Subscribe(_ string, _ byte, _ func(string, []byte)) error { return nil }

func (m *hintMockMQTT) IsConnected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connected
}

func (m *hintMockMQTT) getPublished() []mqttMsg {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.published
}

func TestRouteHintPublisherPublishes(t *testing.T) {
	router := NewRouter(DefaultRouteTTL)

	// Seed routes.
	var dest1, dest2 [TruncatedHashLen]byte
	dest1[0] = 0x01
	dest2[0] = 0x02
	router.mu.Lock()
	router.routes[dest1] = &RouteEntry{
		DestHash:  dest1,
		Interface: IfaceMQTT,
		Cost:      0,
		Hops:      1,
		LastSeen:  time.Now(),
		ExpiresAt: time.Now().Add(30 * time.Minute),
	}
	router.routes[dest2] = &RouteEntry{
		DestHash:  dest2,
		Interface: IfaceIridium,
		Cost:      0.05,
		Hops:      3,
		LastSeen:  time.Now(),
		ExpiresAt: time.Now().Add(30 * time.Minute),
	}
	router.mu.Unlock()

	mqtt := &hintMockMQTT{connected: true}
	cfg := DefaultRouteHintPublisherConfig()
	pub := NewRouteHintPublisher(router, mqtt, nil, cfg)

	// Trigger immediate publish.
	pub.PublishNow()

	msgs := mqtt.getPublished()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 published message, got %d", len(msgs))
	}

	msg := msgs[0]
	if msg.topic != ReticulumRoutesTopic {
		t.Errorf("topic: got %q, want %q", msg.topic, ReticulumRoutesTopic)
	}
	if !msg.retained {
		t.Error("expected retained=true for routing hints")
	}

	// Parse the published JSON.
	var hint RouteHintMessage
	if err := json.Unmarshal(msg.payload, &hint); err != nil {
		t.Fatal(err)
	}
	if len(hint.Routes) != 2 {
		t.Errorf("routes: got %d, want 2", len(hint.Routes))
	}
	if hint.Timestamp == "" {
		t.Error("expected non-empty timestamp")
	}

	if pub.PublishedCount() != 1 {
		t.Errorf("published count: got %d, want 1", pub.PublishedCount())
	}
}

func TestRouteHintPublisherSkipsEmpty(t *testing.T) {
	router := NewRouter(DefaultRouteTTL)
	mqtt := &hintMockMQTT{connected: true}
	cfg := DefaultRouteHintPublisherConfig()
	pub := NewRouteHintPublisher(router, mqtt, nil, cfg)

	pub.PublishNow()

	msgs := mqtt.getPublished()
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages when no routes, got %d", len(msgs))
	}
}

func TestRouteHintPublisherSkipsDisconnected(t *testing.T) {
	router := NewRouter(DefaultRouteTTL)
	var dest [TruncatedHashLen]byte
	dest[0] = 0x01
	router.mu.Lock()
	router.routes[dest] = &RouteEntry{
		DestHash:  dest,
		Interface: IfaceMQTT,
		LastSeen:  time.Now(),
		ExpiresAt: time.Now().Add(30 * time.Minute),
	}
	router.mu.Unlock()

	mqtt := &hintMockMQTT{connected: false}
	cfg := DefaultRouteHintPublisherConfig()
	pub := NewRouteHintPublisher(router, mqtt, nil, cfg)

	pub.PublishNow()

	msgs := mqtt.getPublished()
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages when disconnected, got %d", len(msgs))
	}
}
