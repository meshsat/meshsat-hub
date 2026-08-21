package paho

import (
	"errors"
	"sync"
	"testing"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
)

// --- test doubles -----------------------------------------------------------

type fakeToken struct{ err error }

func (t *fakeToken) Wait() bool                     { return true }
func (t *fakeToken) WaitTimeout(time.Duration) bool { return true }
func (t *fakeToken) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
func (t *fakeToken) Error() error { return t.err }

type subscribeCall struct {
	topic string
	qos   byte
}

// fakeClient mimics the one Paho behaviour this bug hinges on: the client
// router keeps a single callback per exact topic filter, so a repeated
// Subscribe on the same filter replaces the previous callback (MESHSAT-710).
type fakeClient struct {
	mu        sync.Mutex
	calls     []subscribeCall
	callbacks map[string]pahomqtt.MessageHandler
	subErr    error
}

func newFakeClient() *fakeClient {
	return &fakeClient{callbacks: make(map[string]pahomqtt.MessageHandler)}
}

func (c *fakeClient) Subscribe(topic string, qos byte, cb pahomqtt.MessageHandler) pahomqtt.Token {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.subErr != nil {
		return &fakeToken{err: c.subErr}
	}
	c.calls = append(c.calls, subscribeCall{topic: topic, qos: qos})
	c.callbacks[topic] = cb // last Subscribe wins, like Paho's router.addRoute
	return &fakeToken{}
}

func (c *fakeClient) subscribeCalls() []subscribeCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]subscribeCall, len(c.calls))
	copy(out, c.calls)
	return out
}

// deliver feeds a message through the callback stored for a filter, as the
// broker and Paho router would.
func (c *fakeClient) deliver(t *testing.T, filter, topic string, payload []byte) {
	t.Helper()
	c.mu.Lock()
	cb := c.callbacks[filter]
	c.mu.Unlock()
	if cb == nil {
		t.Fatalf("no callback registered for filter %s", filter)
	}
	cb(c, &fakeMessage{topic: topic, payload: payload})
}

// Remaining pahomqtt.Client methods — unused by Subscribe/resubscribe.
func (c *fakeClient) IsConnected() bool       { return true }
func (c *fakeClient) IsConnectionOpen() bool  { return true }
func (c *fakeClient) Connect() pahomqtt.Token { return &fakeToken{} }
func (c *fakeClient) Disconnect(uint)         {}
func (c *fakeClient) Publish(string, byte, bool, interface{}) pahomqtt.Token {
	return &fakeToken{}
}
func (c *fakeClient) SubscribeMultiple(map[string]byte, pahomqtt.MessageHandler) pahomqtt.Token {
	return &fakeToken{}
}
func (c *fakeClient) Unsubscribe(...string) pahomqtt.Token     { return &fakeToken{} }
func (c *fakeClient) AddRoute(string, pahomqtt.MessageHandler) {}
func (c *fakeClient) OptionsReader() pahomqtt.ClientOptionsReader {
	return pahomqtt.ClientOptionsReader{}
}

type fakeMessage struct {
	topic   string
	payload []byte
}

func (m *fakeMessage) Duplicate() bool   { return false }
func (m *fakeMessage) Qos() byte         { return 1 }
func (m *fakeMessage) Retained() bool    { return false }
func (m *fakeMessage) Topic() string     { return m.topic }
func (m *fakeMessage) MessageID() uint16 { return 0 }
func (m *fakeMessage) Payload() []byte   { return m.payload }
func (m *fakeMessage) Ack()              {}

// --- tests ------------------------------------------------------------------

// Two components subscribing to the same filter must BOTH receive messages,
// through a single broker subscription. This is the MESHSAT-710 regression:
// previously the second Subscribe replaced the first handler in Paho's router.
func TestSubscribeSameFilterFansOutToAllHandlers(t *testing.T) {
	fc := newFakeClient()
	b := &Bus{inner: fc}

	var got1, got2 []string
	if err := b.Subscribe("meshsat/+/mo/decoded", 1, func(topic string, _ []byte) {
		got1 = append(got1, topic)
	}); err != nil {
		t.Fatalf("first subscribe: %v", err)
	}
	if err := b.Subscribe("meshsat/+/mo/decoded", 1, func(topic string, _ []byte) {
		got2 = append(got2, topic)
	}); err != nil {
		t.Fatalf("second subscribe: %v", err)
	}

	if n := len(fc.subscribeCalls()); n != 1 {
		t.Fatalf("expected 1 broker subscription for the shared filter, got %d", n)
	}

	fc.deliver(t, "meshsat/+/mo/decoded", "meshsat/dev1/mo/decoded", []byte(`{"imei":"dev1"}`))

	if len(got1) != 1 || len(got2) != 1 {
		t.Fatalf("both handlers must receive the message: handler1=%d handler2=%d (last-Subscribe-wins regression)",
			len(got1), len(got2))
	}
	if got1[0] != "meshsat/dev1/mo/decoded" || got2[0] != "meshsat/dev1/mo/decoded" {
		t.Fatalf("handlers must see the concrete topic: %q / %q", got1[0], got2[0])
	}
}

// A later subscriber asking for a higher QoS upgrades the shared broker
// subscription; a later lower-QoS subscriber rides the existing one.
func TestSubscribeQoSUpgradeResubscribes(t *testing.T) {
	fc := newFakeClient()
	b := &Bus{inner: fc}

	var n0, n1 int
	if err := b.Subscribe("meshsat/+/position", 0, func(string, []byte) { n0++ }); err != nil {
		t.Fatal(err)
	}
	if err := b.Subscribe("meshsat/+/position", 1, func(string, []byte) { n1++ }); err != nil {
		t.Fatal(err)
	}

	calls := fc.subscribeCalls()
	if len(calls) != 2 {
		t.Fatalf("QoS upgrade must resubscribe: got %d broker calls", len(calls))
	}
	if calls[1].qos != 1 {
		t.Fatalf("upgraded subscription must use QoS 1, got %d", calls[1].qos)
	}

	if err := b.Subscribe("meshsat/+/position", 0, func(string, []byte) {}); err != nil {
		t.Fatal(err)
	}
	if len(fc.subscribeCalls()) != 2 {
		t.Fatalf("lower-QoS subscriber must not resubscribe, got %d broker calls", len(fc.subscribeCalls()))
	}

	fc.deliver(t, "meshsat/+/position", "meshsat/dev1/position", []byte(`{"lat":1}`))
	if n0 != 1 || n1 != 1 {
		t.Fatalf("both original handlers must receive after upgrade: n0=%d n1=%d", n0, n1)
	}
}

// After a reconnect, resubscribe must replay one dispatcher per filter and
// every registered handler must keep receiving.
func TestResubscribeReplaysEveryFilterWithFanOut(t *testing.T) {
	fc := newFakeClient()
	b := &Bus{inner: fc}

	var n1, n2, n3 int
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(b.Subscribe("meshsat/+/mo/decoded", 1, func(string, []byte) { n1++ }))
	must(b.Subscribe("meshsat/+/mo/decoded", 1, func(string, []byte) { n2++ }))
	must(b.Subscribe("meshsat/+/position", 1, func(string, []byte) { n3++ }))

	// Fresh client simulates the reconnect: Paho's router state is gone.
	fc2 := newFakeClient()
	b.resubscribe(fc2)

	if n := len(fc2.subscribeCalls()); n != 2 {
		t.Fatalf("expected 2 replayed filters, got %d", n)
	}

	fc2.deliver(t, "meshsat/+/mo/decoded", "meshsat/x/mo/decoded", []byte(`{}`))
	fc2.deliver(t, "meshsat/+/position", "meshsat/x/position", []byte(`{"lat":1}`))

	if n1 != 1 || n2 != 1 || n3 != 1 {
		t.Fatalf("all handlers must survive reconnect: n1=%d n2=%d n3=%d", n1, n2, n3)
	}
}

// A failed broker subscribe must leave no registration behind, and a retry
// after the broker recovers must work from a clean slate.
func TestSubscribeErrorLeavesNoRegistration(t *testing.T) {
	fc := newFakeClient()
	fc.subErr = errors.New("broker down")
	b := &Bus{inner: fc}

	if err := b.Subscribe("meshsat/+/sos", 1, func(string, []byte) {}); err == nil {
		t.Fatal("expected subscribe error")
	}

	fc.mu.Lock()
	fc.subErr = nil
	fc.mu.Unlock()

	var n int
	if err := b.Subscribe("meshsat/+/sos", 1, func(string, []byte) { n++ }); err != nil {
		t.Fatal(err)
	}
	fc.deliver(t, "meshsat/+/sos", "meshsat/dev1/sos", []byte(`{"triggered":true}`))
	if n != 1 {
		t.Fatalf("retry handler must receive, got %d", n)
	}
}
