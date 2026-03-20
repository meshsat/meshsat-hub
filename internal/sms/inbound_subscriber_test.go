package sms

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/bus"
)

// inboundMockBus implements bus.MessageBus for inbound subscriber tests.
type inboundMockBus struct {
	handlers map[string]bus.MessageHandler
}

func newInboundMockBus() *inboundMockBus {
	return &inboundMockBus{handlers: make(map[string]bus.MessageHandler)}
}

func (m *inboundMockBus) Connect() error                            { return nil }
func (m *inboundMockBus) Disconnect()                               {}
func (m *inboundMockBus) IsConnected() bool                         { return true }
func (m *inboundMockBus) Publish(string, byte, bool, []byte) error  { return nil }
func (m *inboundMockBus) PublishJSON(string, byte, bool, any) error { return nil }
func (m *inboundMockBus) QueueSubscribe(string, byte, string, bus.MessageHandler) error {
	return nil
}
func (m *inboundMockBus) Subscribe(topic string, _ byte, handler bus.MessageHandler) error {
	m.handlers[topic] = handler
	return nil
}

func (m *inboundMockBus) fire(topic string, payload []byte) {
	for pattern, h := range m.handlers {
		if pattern == "meshsat/+/sms/inbound" {
			h(topic, payload)
		}
	}
}

func TestInboundSubscriber_Start(t *testing.T) {
	mb := newInboundMockBus()
	sub := NewInboundSubscriber(mb, nil, "default")
	if err := sub.Start(); err != nil {
		t.Fatal(err)
	}
	if _, ok := mb.handlers["meshsat/+/sms/inbound"]; !ok {
		t.Error("expected subscription to meshsat/+/sms/inbound")
	}
}

func TestInboundSubscriber_HandleInbound_ValidPayload(t *testing.T) {
	mb := newInboundMockBus()
	// store is nil — handleInbound will fail on persist but should not panic.
	sub := NewInboundSubscriber(mb, nil, "default")
	_ = sub.Start()

	payload := InboundMQTTPayload{
		From:      "+31612345678",
		Body:      "Hello from Android",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	data, _ := json.Marshal(payload)

	// Should not panic even without a store.
	mb.fire("meshsat/android-001/sms/inbound", data)
}

func TestInboundSubscriber_HandleInbound_EmptyBody(t *testing.T) {
	mb := newInboundMockBus()
	sub := NewInboundSubscriber(mb, nil, "default")
	_ = sub.Start()

	payload := InboundMQTTPayload{From: "+31612345678", Body: ""}
	data, _ := json.Marshal(payload)

	// Should log warning but not panic.
	mb.fire("meshsat/android-001/sms/inbound", data)
}

func TestInboundSubscriber_HandleInbound_InvalidJSON(t *testing.T) {
	mb := newInboundMockBus()
	sub := NewInboundSubscriber(mb, nil, "default")
	_ = sub.Start()

	// Should log warning but not panic.
	mb.fire("meshsat/android-001/sms/inbound", []byte("not-json"))
}

func TestInboundMQTTPayload_Serialization(t *testing.T) {
	p := InboundMQTTPayload{
		From:      "+31612345678",
		To:        "+3197010258258",
		Body:      "Test message",
		Timestamp: "2026-03-20T10:30:00Z",
	}

	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}

	var p2 InboundMQTTPayload
	if err := json.Unmarshal(data, &p2); err != nil {
		t.Fatal(err)
	}

	if p2.From != p.From || p2.Body != p.Body || p2.Timestamp != p.Timestamp {
		t.Error("roundtrip mismatch")
	}
}
