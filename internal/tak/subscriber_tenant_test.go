package tak

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
)

type stubPlatform func(topic string) bool

func (f stubPlatform) IsPlatformTopic(_ context.Context, topic string) bool { return f(topic) }

// notCustomer treats every topic naming the tenant t_cust as a customer's.
var notCustomer = stubPlatform(func(topic string) bool { return !strings.Contains(topic, "t_cust") })

// recordingBus keeps each subscription's handler and delivers a topic to
// every filter it matches ('+' only, which is all the subscriber uses).
type recordingBus struct{ handlers map[string]bus.MessageHandler }

func newRecordingBus() *recordingBus {
	return &recordingBus{handlers: map[string]bus.MessageHandler{}}
}

func (b *recordingBus) Connect() error                            { return nil }
func (b *recordingBus) Publish(string, byte, bool, []byte) error  { return nil }
func (b *recordingBus) PublishJSON(string, byte, bool, any) error { return nil }
func (b *recordingBus) IsConnected() bool                         { return true }
func (b *recordingBus) Disconnect()                               {}
func (b *recordingBus) Subscribe(topic string, _ byte, h bus.MessageHandler) error {
	b.handlers[topic] = h
	return nil
}
func (b *recordingBus) QueueSubscribe(topic string, q byte, _ string, h bus.MessageHandler) error {
	return b.Subscribe(topic, q, h)
}

func (b *recordingBus) deliver(topic string, payload []byte) {
	for filter, h := range b.handlers {
		if filterMatches(filter, topic) {
			h(topic, payload)
		}
	}
}

func filterMatches(filter, topic string) bool {
	f, tp := strings.Split(filter, "/"), strings.Split(topic, "/")
	if len(f) != len(tp) {
		return false
	}
	for i := range f {
		if f[i] != "+" && f[i] != tp[i] {
			return false
		}
	}
	return true
}

// readUntil reads from conn until marker has arrived, and returns everything
// read. Raw Read with a deadline, never bufio.Scanner (MESHSAT-697).
func readUntil(t *testing.T, conn net.Conn, marker string) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var got strings.Builder
	buf := make([]byte, 4096)
	for !strings.Contains(got.String(), marker) {
		n, err := conn.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			t.Fatalf("read before %q arrived: %v (got %q)", marker, err, got.String())
		}
	}
	return got.String()
}

// The TAK server is the operator's, and every ATAK client on it sees what
// arrives. A customer's position, SOS, telemetry and message text must never
// be sent there; the platform's still are. [MESHSAT-1032]
func TestCustomerTrafficNeverReachesTheTAKServer(t *testing.T) {
	ln, conns := startSilentServer(t)
	defer func() { _ = ln.Close() }()
	client := clientFor(t, ln)
	if err := client.Connect(); err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect()
	var server net.Conn
	select {
	case server = <-conns:
		defer func() { _ = server.Close() }()
	case <-time.After(2 * time.Second):
		t.Fatal("client never connected")
	}

	b := newRecordingBus()
	if err := NewSubscriber(b, client, notCustomer, "", 0).Start(); err != nil {
		t.Fatal(err)
	}

	const customer, platform = "300234069999777", "300234063904190"
	// Customer traffic first. TCP keeps order, so anything of it that had been
	// sent would arrive before the platform's position does.
	b.deliver("meshsat/t_cust/"+customer+"/position", []byte(`{"lat":52.37,"lon":4.90}`))
	b.deliver("meshsat/t_cust/"+customer+"/sos", []byte(`{"triggered":true,"lat":52.37,"lon":4.90}`))
	b.deliver("meshsat/t_cust/"+customer+"/telemetry", []byte(`{"battery":80,"lat":52.37,"lon":4.90}`))
	b.deliver("meshsat/t_cust/"+customer+"/mo/decoded", []byte(`{"text":"customer secret","iridium_latitude":52.37,"iridium_longitude":4.90}`))
	b.deliver("meshsat/"+platform+"/position", []byte(`{"lat":52.37,"lon":4.90}`))

	got := readUntil(t, server, platform)
	for _, leaked := range []string{customer, "customer secret", "MESHSAT-HUB-9777"} {
		if strings.Contains(got, leaked) {
			t.Errorf("customer data %q reached the TAK server", leaked)
		}
	}
}

// No subscription may bypass the gate: every handler the subscriber
// registers must ask the platform checker before doing anything.
func TestEveryTAKSubscriptionIsGated(t *testing.T) {
	var asked []string
	refuse := stubPlatform(func(topic string) bool {
		asked = append(asked, topic)
		return false
	})
	b := newRecordingBus()
	if err := NewSubscriber(b, NewClient("127.0.0.1", 1, false), refuse, "", 0).Start(); err != nil {
		t.Fatal(err)
	}
	if len(b.handlers) == 0 {
		t.Fatal("no subscriptions")
	}
	for filter, h := range b.handlers {
		topic := strings.ReplaceAll(filter, "+", "x")
		before := len(asked)
		h(topic, []byte(`{}`))
		if len(asked) != before+1 {
			t.Errorf("subscription %q handled a message without asking the platform checker", filter)
		}
	}
}

func TestTAKStartRefusesWithoutAPlatformChecker(t *testing.T) {
	b := newRecordingBus()
	if err := NewSubscriber(b, NewClient("127.0.0.1", 1, false), nil, "", 0).Start(); err == nil {
		t.Fatal("Start succeeded with no platform checker")
	}
	if len(b.handlers) != 0 {
		t.Errorf("%d subscriptions made before refusing", len(b.handlers))
	}
}

// Federation is off in production (MESHSAT-1031), but if it is ever turned
// back on it must follow the same rule. A net.Pipe stands in for a peer.
func TestFederationForwardsOnlyPlatformTraffic(t *testing.T) {
	hubEnd, peerEnd := net.Pipe()
	defer func() { _ = hubEnd.Close(); _ = peerEnd.Close() }()

	f := NewFederation(FederationConfig{}, nil)
	f.running.Store(true)
	f.SetPlatformChecker(notCustomer)
	p := &federationPeer{addr: "pipe", conn: hubEnd}
	p.connected.Store(true)
	f.peers = append(f.peers, p)

	const customer, platform = "300234069999777", "300234063904190"
	received := make(chan string, 1)
	go func() {
		received <- readUntil(t, peerEnd, platform)
	}()
	f.handleMQTTForFederation("meshsat/t_cust/"+customer+"/position", []byte(`{"lat":52.37,"lon":4.90}`))
	f.handleMQTTForFederation("meshsat/"+platform+"/position", []byte(`{"lat":52.37,"lon":4.90}`))

	if got := <-received; strings.Contains(got, customer) {
		t.Errorf("customer device %s was federated", customer)
	}
}

func TestFederationWithoutAPlatformCheckerFederatesNothing(t *testing.T) {
	hubEnd, peerEnd := net.Pipe()
	defer func() { _ = hubEnd.Close(); _ = peerEnd.Close() }()

	f := NewFederation(FederationConfig{}, nil)
	f.running.Store(true)
	p := &federationPeer{addr: "pipe", conn: hubEnd}
	p.connected.Store(true)
	f.peers = append(f.peers, p)

	f.handleMQTTForFederation("meshsat/300234063904190/position", []byte(`{"lat":52.37,"lon":4.90}`))
	if _, out, _ := f.Stats(); out != 0 {
		t.Errorf("%d CoT events federated with no platform checker", out)
	}
}
