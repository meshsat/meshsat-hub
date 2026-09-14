package aprsis

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
)

// stubTenants answers which tenant owns a topic, standing in for
// *tenancy.Resolver.
type stubTenants func(topic string) string

func (f stubTenants) TenantForTopic(_ context.Context, topic string) string { return f(topic) }

// recordingBus keeps each subscription's handler and delivers a topic to
// every filter it matches ('+' only, which is all the subscriber uses).
type recordingBus struct {
	handlers map[string]bus.MessageHandler
	mu       sync.Mutex
	posted   []string
}

func newRecordingBus() *recordingBus {
	return &recordingBus{handlers: map[string]bus.MessageHandler{}}
}

func (b *recordingBus) Connect() error                           { return nil }
func (b *recordingBus) Publish(string, byte, bool, []byte) error { return nil }
func (b *recordingBus) PublishJSON(topic string, _ byte, _ bool, _ any) error {
	b.mu.Lock()
	b.posted = append(b.posted, topic)
	b.mu.Unlock()
	return nil
}
func (b *recordingBus) IsConnected() bool { return true }
func (b *recordingBus) Disconnect()       {}
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

// wiredClient returns a Client that behaves as connected and records every
// packet it transmits, without touching the network. The APRS-IS wire is a
// plain line protocol, so a net.Pipe is a faithful stand-in and lets a test
// assert the thing that actually matters: WHICH CALLSIGN a position went out
// under.
func wiredClient(t *testing.T, callsign string, ssid int) (*Client, func() []string) {
	t.Helper()
	ours, theirs := net.Pipe()
	c := NewClient("test:14580", callsign, ssid, "12345", "")
	c.conn = ours
	c.running.Store(true)

	var mu sync.Mutex
	var lines []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(theirs)
		for sc.Scan() {
			mu.Lock()
			lines = append(lines, sc.Text())
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		c.running.Store(false)
		_ = ours.Close()
		_ = theirs.Close()
		<-done
	})

	return c, func() []string {
		// Give the reader a moment to see the write; net.Pipe is synchronous on
		// the write side, so this only covers the scanner's own scheduling.
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

// poolWith seats already-connected clients per tenant, the state Reconcile
// would have produced. Reconcile itself needs a store and a network; routing is
// what these tests are about.
//
// platform is passed in and NOT left nil, which matters more than it looks: with
// a nil platform field, a mutation that makes ForTenant fall back to the
// operator's callsign returns nil anyway and every test still passes. The first
// version of this helper did exactly that, and the mutation went uncaught.
func poolWith(platform *Client, clients map[string]*Client) *ConnPool {
	p := NewConnPool(platform, nil, "default")
	for tid, c := range clients {
		p.conns[tid] = &conn{fingerprint: "test", client: c}
	}
	return p
}

// The property MESHSAT-1032 established and MESHSAT-1121 must not lose: a
// tenant's position is NEVER put on a public network under a callsign that is
// not theirs. What changed is the remedy -- a tenant with its own licence now
// transmits under it, instead of being dropped -- so this asserts both halves at
// once, and asserts them on the WIRE rather than on an internal flag.
func TestEachTenantTransmitsUnderItsOwnCallsignAndNobodyElsesEver(t *testing.T) {
	const platformDev, tenantDev, strangerDev = "300234063904190", "300234069999777", "300234069999778"

	b := newRecordingBus()
	platformClient, platformSent := wiredClient(t, "PI1MSH", 10)
	tenantClient, tenantSent := wiredClient(t, "PD1ABC", 7)

	// t_cust brought its own licence. t_none did not.
	pool := poolWith(platformClient, map[string]*Client{
		"default": platformClient,
		"t_cust":  tenantClient,
	})
	s := NewSubscriber(b, pool, stubTenants(func(topic string) string {
		switch {
		case strings.Contains(topic, "t_cust"):
			return "t_cust"
		case strings.Contains(topic, "t_none"):
			return "t_none"
		default:
			return "default"
		}
	}), 60)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}

	pos := []byte(`{"lat":52.37,"lon":4.90,"source":"iridium"}`)
	b.deliver("meshsat/"+platformDev+"/position", pos)
	b.deliver("meshsat/t_cust/"+tenantDev+"/position", pos)
	b.deliver("meshsat/t_none/"+strangerDev+"/position", pos)

	plat, tenant := platformSent(), tenantSent()

	if len(plat) != 1 {
		t.Fatalf("the platform callsign transmitted %d packets, want exactly 1: %q", len(plat), plat)
	}
	if !strings.HasPrefix(plat[0], "PI1MSH-10>") {
		t.Errorf("platform packet not sent under PI1MSH-10: %q", plat[0])
	}
	if len(tenant) != 1 {
		t.Fatalf("the tenant callsign transmitted %d packets, want exactly 1: %q", len(tenant), tenant)
	}
	if !strings.HasPrefix(tenant[0], "PD1ABC-7>") {
		t.Errorf("tenant packet not sent under its own PD1ABC-7: %q", tenant[0])
	}

	// The heart of it: t_none has no licence on file, so its position goes
	// NOWHERE. Not under the operator's callsign, which is what MESHSAT-1032
	// contained, and not under another customer's either. Three positions were
	// delivered and exactly two packets left, so the third reached no radio.
	if len(plat)+len(tenant) != 2 {
		t.Fatalf("a tenant with no APRS account of its own was transmitted: %q %q", plat, tenant)
	}
}

// A tenant that configured nothing must not inherit the operator's callsign.
// ForTenant answering nil is what enforces it, and it is asserted directly
// because it is one `if` away from becoming a fallback during a refactor.
func TestATenantWithNoAccountGetsNoClientRatherThanThePlatformsOne(t *testing.T) {
	platformClient, _ := wiredClient(t, "PI1MSH", 10)
	pool := poolWith(platformClient, map[string]*Client{"default": platformClient})

	if got := pool.ForTenant("default"); got == nil {
		t.Fatal("the default tenant did not get the platform client")
	}
	if got := pool.ForTenant("t_cust"); got != nil {
		t.Fatalf("a tenant with no APRS account was handed the callsign %q: "+
			"a callsign is a licence issued to a person, and this would put one "+
			"customer's positions on a public network under somebody else's licence",
			got.FormatCallsign())
	}
}

// An APRS message addressed to one tenant's callsign arrives on that tenant's
// own socket, and must surface only in that tenant's topic space.
func TestInboundMessagesLandInTheirOwnTenantsTopic(t *testing.T) {
	b := newRecordingBus()
	pool := poolWith(nil, map[string]*Client{})
	s := NewSubscriber(b, pool, stubTenants(func(string) string { return "default" }), 60)

	s.handleInboundPacket("t_cust", "PD1ABC", "N0CALL>APRS,TCPIP*::PD1ABC   :hello{1")

	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.posted) != 1 {
		t.Fatalf("published %d messages, want 1: %v", len(b.posted), b.posted)
	}
	if b.posted[0] != "meshsat/t_cust/hub/aprsis/inbound" {
		t.Errorf("inbound message published to %q; every tenant's APRS traffic "+
			"would share one topic and each could read the others'", b.posted[0])
	}
}

func TestAPRSStartRefusesWithoutATenantResolver(t *testing.T) {
	b := newRecordingBus()
	s := NewSubscriber(b, poolWith(nil, map[string]*Client{}), nil, 60)
	if err := s.Start(); err == nil {
		t.Fatal("Start succeeded with no tenant resolver")
	}
	if len(b.handlers) != 0 {
		t.Errorf("%d subscriptions made before refusing", len(b.handlers))
	}
}
