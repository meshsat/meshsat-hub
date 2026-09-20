package routing

import (
	"context"
	"encoding/json"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
	"sync"
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
	mu    sync.Mutex
	saw   []string // the tenant each Fire was given
}

func (m *mockWebhookFirer) Fire(tenantID string, _ webhook.EventType, _, _ string, _ json.RawMessage) {
	m.fired.Add(1)
	m.mu.Lock()
	m.saw = append(m.saw, tenantID)
	m.mu.Unlock()
}

func (m *mockWebhookFirer) tenants() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.saw...)
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
	// The engine always puts the message's tenant on the handler context.
	h(tenancy.WithTenant(context.Background(), "t_alpha"), nil, "dev1", testPayload())
	if firer.fired.Load() != 1 {
		t.Error("expected webhook to fire")
	}
	if got := firer.tenants(); len(got) != 1 || got[0] != "t_alpha" {
		t.Errorf("the handler passed tenant %v, want [t_alpha]. It used to discard the "+
			"context and fire at every tenant's webhooks (MESHSAT-1118).", got)
	}
}

// A routed message that somehow arrives with no tenant must fire nothing.
// Firing would mean choosing a tenant, and the only choice available is
// "everyone", which is the bug this replaced.
func TestWebhookHandlerFiresNothingWithoutATenant(t *testing.T) {
	firer := &mockWebhookFirer{}
	h := NewWebhookHandler(firer)
	h(context.Background(), nil, "dev1", testPayload())
	if firer.fired.Load() != 0 {
		t.Errorf("fired %d times with no tenant on the context; want 0", firer.fired.Load())
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

// The satellite destination sends "[origin] text" to every modem IMEI in
// the route filter except the origin itself (MESHSAT-964 D).
func TestNewSatelliteHandler(t *testing.T) {
	var got []string
	h := NewSatelliteHandler(func(_ context.Context, tenantID, imei, text string) error {
		got = append(got, tenantID+"|"+imei+"|"+text)
		return nil
	})
	route := &store.Route{ID: "r1", Filter: "300000000000003, 300000000000002"}
	payload, _ := json.Marshal(map[string]any{"text": "meet at the booth"})
	h(tenancy.WithTenant(context.Background(), "t1"), route, "300000000000002", payload)
	if len(got) != 1 || got[0] != "t1|300000000000003|[300000000000002] meet at the booth" {
		t.Fatalf("sends = %v", got)
	}
	got = nil
	h(context.Background(), &store.Route{ID: "r2"}, "x", payload)
	if len(got) != 0 {
		t.Fatalf("empty filter sent %v", got)
	}
}

// Kit to kit over satellite (MESHSAT-1282): the relay forwards the ORIGINAL
// payload, not the text. The receiving Bridge authenticates the bytes with a
// key the Hub does not hold, so the "[imei] " prefix the text handler adds, or
// anything else, makes the message undeliverable.
func TestSatelliteRelayForwardsTheWireUntouched(t *testing.T) {
	const kitA, kitB = "300000000000001", "300000000000002"
	const wire = "AXN5bnRoZXRpYy1jaXBoZXJ0ZXh0LWJhc2U2NA==" // stands for 0x01 + base64 ciphertext; synthetic
	type sent struct{ tenant, imei, wire string }
	var got []sent
	h := NewSatelliteRelayHandler(func(_ context.Context, tenantID, imei, w string) error {
		got = append(got, sent{tenantID, imei, w})
		return nil
	})
	payload, _ := json.Marshal(map[string]any{"text": "c3ludGhldGljLWNpcGhlcnRleHQ=", "wire": wire, "opaque": true})

	// Both kits listed, as a symmetric pair of routes would: never back to the origin.
	route := &store.Route{ID: "r1", Name: "kits over satellite", Filter: kitA + ", " + kitB}
	h(context.Background(), route, kitA, payload)
	if len(got) != 1 {
		t.Fatalf("sends = %+v, want exactly one (to kitB)", got)
	}
	if got[0].imei != kitB {
		t.Errorf("relayed to %s, want %s", got[0].imei, kitB)
	}
	if got[0].wire != wire {
		t.Errorf("wire changed in transit:\n got %q\nwant %q", got[0].wire, wire)
	}
	if got[0].tenant != store.DefaultTenantID {
		t.Errorf("tenant = %q", got[0].tenant)
	}

	// No original payload on the message: nothing is sent, least of all the text.
	got = nil
	noWire, _ := json.Marshal(map[string]any{"text": "hello"})
	h(context.Background(), route, kitA, noWire)
	if len(got) != 0 {
		t.Fatalf("relayed without a wire payload: %+v", got)
	}
}

// An opaque payload reaches the destinations that can do something with
// ciphertext, and none that would put it in front of a person or on the air.
func TestOpaqueDestinations(t *testing.T) {
	for dest, want := range map[string]bool{
		DestSatelliteRelay: true, "webhook": true, "mqtt": true, "notification": true,
		"sms": false, "email": false, "satellite": false, "tak": false, "aprs": false,
	} {
		if got := carriesOpaque(dest); got != want {
			t.Errorf("carriesOpaque(%q) = %v, want %v", dest, got, want)
		}
	}
	if !isRecipientDestination(DestSatelliteRelay) {
		t.Error("the relay's filter is its recipient list; it must not be read as a message match")
	}
}
