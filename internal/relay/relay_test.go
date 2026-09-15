package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/meshsat/meshsat-hub/internal/bus"
)

// fakeBus delivers synchronously to every handler whose filter matches, so a
// frame published by one Relay reaches the other in the same call, exactly
// as a broker would deliver it to every subscribed replica.
type fakeBus struct {
	mu   sync.Mutex
	subs []struct {
		filter string
		h      bus.MessageHandler
	}
}

func (b *fakeBus) Connect() error                            { return nil }
func (b *fakeBus) IsConnected() bool                         { return true }
func (b *fakeBus) Disconnect()                               {}
func (b *fakeBus) PublishJSON(string, byte, bool, any) error { return nil }
func (b *fakeBus) Subscribe(filter string, _ byte, h bus.MessageHandler) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = append(b.subs, struct {
		filter string
		h      bus.MessageHandler
	}{filter, h})
	return nil
}
func (b *fakeBus) QueueSubscribe(f string, q byte, _ string, h bus.MessageHandler) error {
	return b.Subscribe(f, q, h)
}
func (b *fakeBus) Publish(topic string, _ byte, _ bool, payload []byte) error {
	if strings.ContainsAny(topic, "+#") {
		panic("publish to a wildcard topic: " + topic)
	}
	b.mu.Lock()
	subs := append([]struct {
		filter string
		h      bus.MessageHandler
	}(nil), b.subs...)
	b.mu.Unlock()
	for _, s := range subs {
		if matchFilter(s.filter, topic) {
			s.h(topic, payload)
		}
	}
	return nil
}

func matchFilter(filter, topic string) bool {
	f, t := strings.Split(filter, "/"), strings.Split(topic, "/")
	if len(f) != len(t) {
		return false
	}
	for i := range f {
		if f[i] != "+" && f[i] != t[i] {
			return false
		}
	}
	return true
}

// twoReplicas stands up relay A serving bridges and relay B serving clients,
// on one bus, the way round-robin lands the two ends on different pods.
func twoReplicas(t *testing.T, opt Options) (bridgeURL, clientURL string, a, b *Relay) {
	t.Helper()
	fb := &fakeBus{}
	a, b = New(fb, nil, opt), New(fb, nil, opt)
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	sa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.ServeBridge(w, r, r.URL.Query().Get("tenant"), r.URL.Query().Get("bridge"))
	}))
	sb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		b.ServeClient(w, r, q.Get("tenant"), q.Get("bridge"), q.Get("client"))
	}))
	t.Cleanup(sa.Close)
	t.Cleanup(sb.Close)
	return "ws" + strings.TrimPrefix(sa.URL, "http"), "ws" + strings.TrimPrefix(sb.URL, "http"), a, b
}

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	return c
}

func readBinary(t *testing.T, c *websocket.Conn) []byte {
	t.Helper()
	mt, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if mt != websocket.BinaryMessage {
		t.Fatalf("message type %d, want binary", mt)
	}
	return data
}

func TestFramesCrossBothWaysBetweenTwoReplicas(t *testing.T) {
	bURL, cURL, _, _ := twoReplicas(t, Options{})
	bridge := dial(t, bURL+"/?tenant=t1&bridge=kit-a")
	client := dial(t, cURL+"/?tenant=t1&bridge=kit-a&client=phone-1")

	// client -> bridge arrives in an envelope naming the client
	if err := client.WriteMessage(websocket.BinaryMessage, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	id, payload, err := DecodeEnvelope(readBinary(t, bridge))
	if err != nil || id != "phone-1" || string(payload) != "hello" {
		t.Fatalf("bridge got id=%q payload=%q err=%v", id, payload, err)
	}

	// bridge -> client is the bare payload
	frame, _ := EncodeEnvelope("phone-1", []byte("world"))
	if err := bridge.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		t.Fatal(err)
	}
	if got := readBinary(t, client); string(got) != "world" {
		t.Fatalf("client got %q", got)
	}
}

func TestAClientNeverHearsAnotherClientsTunnel(t *testing.T) {
	bURL, cURL, _, _ := twoReplicas(t, Options{})
	bridge := dial(t, bURL+"/?tenant=t1&bridge=kit-a")
	c1 := dial(t, cURL+"/?tenant=t1&bridge=kit-a&client=phone-1")
	c2 := dial(t, cURL+"/?tenant=t1&bridge=kit-a&client=phone-2")

	for _, c := range []*websocket.Conn{c1, c2} {
		if err := c.WriteMessage(websocket.BinaryMessage, []byte("hi")); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		id, _, _ := DecodeEnvelope(readBinary(t, bridge))
		seen[id] = true
	}
	if !seen["phone-1"] || !seen["phone-2"] {
		t.Fatalf("bridge saw %v", seen)
	}

	// A reply addressed to phone-2 must reach phone-2 and only phone-2.
	frame, _ := EncodeEnvelope("phone-2", []byte("for two"))
	if err := bridge.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		t.Fatal(err)
	}
	if got := readBinary(t, c2); string(got) != "for two" {
		t.Fatalf("phone-2 got %q", got)
	}
	_ = c1.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, data, err := c1.ReadMessage(); err == nil {
		t.Fatalf("phone-1 received %q, a frame addressed to phone-2", data)
	}
}

func TestATenantsBridgeIsNotReachableUnderAnotherTenant(t *testing.T) {
	bURL, cURL, _, _ := twoReplicas(t, Options{})
	bridge := dial(t, bURL+"/?tenant=t1&bridge=kit-a")
	// Same bridge id, other tenant: a different rendezvous, not this bridge.
	other := dial(t, cURL+"/?tenant=t2&bridge=kit-a&client=phone-9")
	if err := other.WriteMessage(websocket.BinaryMessage, []byte("wrong door")); err != nil {
		t.Fatal(err)
	}
	_ = bridge.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, data, err := bridge.ReadMessage(); err == nil {
		t.Fatalf("tenant t1's bridge received %q from tenant t2", data)
	}
}

func TestTheFrameAfterTheBudgetClosesWith1008(t *testing.T) {
	bURL, cURL, _, _ := twoReplicas(t, Options{FramesPerMinute: 3})
	bridge := dial(t, bURL+"/?tenant=t1&bridge=kit-a")
	client := dial(t, cURL+"/?tenant=t1&bridge=kit-a&client=phone-1")
	for i := 0; i < 3; i++ {
		if err := client.WriteMessage(websocket.BinaryMessage, []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
		readBinary(t, bridge)
	}
	if err := client.WriteMessage(websocket.BinaryMessage, []byte("one too many")); err != nil {
		t.Fatal(err)
	}
	_, _, err := client.ReadMessage()
	ce, ok := err.(*websocket.CloseError)
	if !ok || ce.Code != CloseBudget {
		t.Fatalf("expected close %d, got %v", CloseBudget, err)
	}
	_ = bridge.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, data, err := bridge.ReadMessage(); err == nil {
		t.Fatalf("the over-budget frame %q was delivered", data)
	}
}

func TestASilentSocketIsClosedAfterThePongDeadline(t *testing.T) {
	bURL, _, a, _ := twoReplicas(t, Options{PingInterval: 20 * time.Millisecond, PongWait: 150 * time.Millisecond})
	c, _, err := websocket.DefaultDialer.Dial(bURL+"/?tenant=t1&bridge=kit-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	// Never call ReadMessage, so the client never answers a ping.
	deadline := time.Now().Add(3 * time.Second)
	for a.Sessions("bridge") != 0 {
		if time.Now().After(deadline) {
			t.Fatal("silent bridge socket still held after the pong deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestANonBinaryFrameIsRefused(t *testing.T) {
	bURL, _, _, _ := twoReplicas(t, Options{})
	bridge := dial(t, bURL+"/?tenant=t1&bridge=kit-a")
	if err := bridge.WriteMessage(websocket.TextMessage, []byte(`{"not":"a frame"}`)); err != nil {
		t.Fatal(err)
	}
	_, _, err := bridge.ReadMessage()
	if ce, ok := err.(*websocket.CloseError); !ok || ce.Code != CloseFrame {
		t.Fatalf("expected close %d, got %v", CloseFrame, err)
	}
}

func TestANewerBridgeSocketSupersedesTheOld(t *testing.T) {
	bURL, cURL, _, _ := twoReplicas(t, Options{})
	old := dial(t, bURL+"/?tenant=t1&bridge=kit-a")
	newer := dial(t, bURL+"/?tenant=t1&bridge=kit-a")
	_, _, err := old.ReadMessage()
	if ce, ok := err.(*websocket.CloseError); !ok || ce.Code != CloseSuperseded {
		t.Fatalf("old socket: expected close %d, got %v", CloseSuperseded, err)
	}
	client := dial(t, cURL+"/?tenant=t1&bridge=kit-a&client=phone-1")
	if err := client.WriteMessage(websocket.BinaryMessage, []byte("to the new one")); err != nil {
		t.Fatal(err)
	}
	id, payload, _ := DecodeEnvelope(readBinary(t, newer))
	if id != "phone-1" || string(payload) != "to the new one" {
		t.Fatalf("newer socket got %q %q", id, payload)
	}
}
