package routing

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/webhook"
)

func TestParseRecipients(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"", 0},
		{"+31612345678", 1},
		{"+31612345678,+14155551234", 2},
		{"+31612345678, +14155551234, +442012345678", 3},
		{"user@example.com", 1},
		{"a@b.com, c@d.com", 2},
		{" , , ", 0},
	}
	for _, tt := range tests {
		got := parseRecipients(tt.input)
		if len(got) != tt.want {
			t.Errorf("parseRecipients(%q) = %d items, want %d", tt.input, len(got), tt.want)
		}
	}
}

func TestFormatRoutedSMS(t *testing.T) {
	// Short message.
	msg := formatRoutedSMS("dev1", "SOS help")
	if msg != "[dev1] SOS help" {
		t.Errorf("got %q", msg)
	}

	// Long message truncated.
	long := ""
	for i := 0; i < 200; i++ {
		long += "x"
	}
	msg = formatRoutedSMS("dev1", long)
	if len(msg) != 160 {
		t.Errorf("len = %d, want 160", len(msg))
	}
	if msg[157:] != "..." {
		t.Errorf("expected trailing ..., got %q", msg[157:])
	}
}

// --- Mock types for destination handler tests ---

type mockWebhookFirer struct {
	fired atomic.Int32
}

func (m *mockWebhookFirer) Fire(_ webhook.EventType, _ string, _ json.RawMessage) {
	m.fired.Add(1)
}

type mockNotifier struct {
	sent atomic.Int32
}

func (m *mockNotifier) Notify(_ context.Context, _ []string, _, _ string) error {
	m.sent.Add(1)
	return nil
}

type mockMQTTPub struct {
	published atomic.Int32
	lastTopic string
}

func (m *mockMQTTPub) Publish(topic string, _ byte, _ bool, _ []byte) error {
	m.published.Add(1)
	m.lastTopic = topic
	return nil
}

func mockRoute(destType, filter string) store.Route {
	return store.Route{ID: "test-1", DestinationType: destType, Filter: filter, Enabled: true}
}

func testPayload() json.RawMessage {
	data, _ := json.Marshal(moDecodedPayload{
		IMEI:    "300234065123456",
		Text:    "Test message from field",
		Channel: "iridium",
	})
	return data
}

func TestNewWebhookHandler(t *testing.T) {
	firer := &mockWebhookFirer{}
	h := NewWebhookHandler(firer)
	h(context.Background(), nil, "dev1", testPayload())
	if firer.fired.Load() != 1 {
		t.Error("expected webhook to fire")
	}
}

func TestNewNotificationHandler(t *testing.T) {
	notifier := &mockNotifier{}
	h := NewNotificationHandler(notifier)
	r := mockRoute("notification", "")
	h(context.Background(), &r, "dev1", testPayload())
	if notifier.sent.Load() != 1 {
		t.Error("expected notification to send")
	}
}

func TestNewMQTTHandler(t *testing.T) {
	pub := &mockMQTTPub{}
	h := NewMQTTHandler(pub)
	h(context.Background(), nil, "dev1", testPayload())
	if pub.published.Load() != 1 {
		t.Error("expected MQTT publish")
	}
	if pub.lastTopic != "meshsat/routed/dev1" {
		t.Errorf("topic: got %s, want meshsat/routed/dev1", pub.lastTopic)
	}
}

func TestNewTAKHandler(t *testing.T) {
	pub := &mockMQTTPub{}
	h := NewTAKHandler(pub)
	h(context.Background(), nil, "dev1", testPayload())
	if pub.published.Load() != 1 {
		t.Error("expected TAK publish")
	}
	if pub.lastTopic != "meshsat/dev1/tak/cot/out" {
		t.Errorf("topic: got %s", pub.lastTopic)
	}
}

func TestNewAPRSHandler(t *testing.T) {
	pub := &mockMQTTPub{}
	h := NewAPRSHandler(pub)
	h(context.Background(), nil, "dev1", testPayload())
	if pub.published.Load() != 1 {
		t.Error("expected APRS publish")
	}
	if pub.lastTopic != "meshsat/dev1/aprs/out" {
		t.Errorf("topic: got %s", pub.lastTopic)
	}
}
