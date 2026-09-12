package takhosted

import (
	"context"
	"net"
	"testing"
	"time"

	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/takfront"
)

// Fan-out (MESHSAT-1065): a tenant may have a hosted instance AND a server of
// their own, and a position must reach every one of them.

// forwarderTo wires a Forwarder at the given upstream addresses, all belonging to
// tenant t1, with a plain TCP dial standing in for the TLS policy (which is tested
// in takfront).
func forwarderTo(t *testing.T, s store.Store, addrs ...string) (*Forwarder, *fakeBus) {
	t.Helper()
	b := newFakeBus()
	var ups []*takfront.Tenant
	for i, a := range addrs {
		label := "external"
		if i == 0 {
			label = "abcdefghij" // the hosted one, by convention in these tests
		}
		ups = append(ups, &takfront.Tenant{TenantID: "t1", Label: label, Upstream: a})
	}
	dial := func(ctx context.Context, tn *takfront.Tenant) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", tn.Upstream)
	}
	lookup := func(_ context.Context, id string) []*takfront.Tenant {
		if id == "t1" {
			return ups
		}
		return nil
	}
	f := NewForwarder(b, s, dial, lookup, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go f.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	return f, b
}

// deadAddress returns an address nothing is listening on: a listener opened to
// reserve the port, then closed.
func deadAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// The headline behaviour: one position, two servers, both receive it.
func TestAPositionReachesEveryUpstream(t *testing.T) {
	hosted := newCapturedOTS(t)
	own := newCapturedOTS(t)
	db := storeWithDevice(t, "300434", "KIT", "rockblock")
	f, b := forwarderTo(t, db, hosted.addr, own.addr)

	topic := hubmqtt.TopicPositionFor("t1", "300434")
	if n := b.deliver(topic, positionPayload(t, "300434", 52.1, 4.5)); n == 0 {
		t.Fatalf("nothing was subscribed to %s", topic)
	}

	if got := hosted.waitFor(t, 1); len(got) != 1 {
		t.Errorf("the hosted instance received %d events, want 1", len(got))
	}
	if got := own.waitFor(t, 1); len(got) != 1 {
		t.Errorf("the tenant's own server received %d events, want 1: adding their own server "+
			"must not mean the position goes to only one of them", len(got))
	}

	// One connection per upstream, not one per tenant: keying by tenant alone would
	// make both upstreams share a connection and send each position to whichever
	// was dialled first.
	f.mu.Lock()
	open := len(f.conns)
	f.mu.Unlock()
	if open != 2 {
		t.Errorf("%d connections open for two upstreams, want 2", open)
	}
}

// A customer's own server being unreachable must not keep their kit off their
// hosted map, and the reverse.
func TestOneDeadUpstreamDoesNotStopTheOther(t *testing.T) {
	hosted := newCapturedOTS(t)
	dead := deadAddress(t)
	db := storeWithDevice(t, "300434", "KIT", "rockblock")

	// The dead one FIRST, so a naive implementation that stopped at the first
	// failure would never reach the live one.
	_, b := forwarderTo(t, db, dead, hosted.addr)

	topic := hubmqtt.TopicPositionFor("t1", "300434")
	if n := b.deliver(topic, positionPayload(t, "300434", 52.1, 4.5)); n == 0 {
		t.Fatalf("nothing was subscribed to %s", topic)
	}

	if got := hosted.waitFor(t, 1); len(got) != 1 {
		t.Fatalf("the reachable upstream received %d events, want 1: a dead upstream listed "+
			"first stopped the rest", len(got))
	}
}

// Both upstreams keep working across several positions, and each holds one
// connection rather than re-dialling: every connection forks a process in
// OpenTAKServer.
func TestBothUpstreamsReuseTheirOwnConnection(t *testing.T) {
	hosted := newCapturedOTS(t)
	own := newCapturedOTS(t)
	db := storeWithDevice(t, "300434", "KIT", "rockblock")
	f, b := forwarderTo(t, db, hosted.addr, own.addr)

	for i := 0; i < 3; i++ {
		b.deliver(hubmqtt.TopicPositionFor("t1", "300434"),
			positionPayload(t, "300434", 52.0+float64(i)/10, 4.5))
	}

	if got := hosted.waitFor(t, 3); len(got) < 3 {
		t.Errorf("hosted received %d of 3", len(got))
	}
	if got := own.waitFor(t, 3); len(got) < 3 {
		t.Errorf("the tenant's own server received %d of 3", len(got))
	}
	f.mu.Lock()
	open := len(f.conns)
	f.mu.Unlock()
	if open != 2 {
		t.Errorf("%d connections for two upstreams after three positions, want 2", open)
	}
}

// The loop guard still applies to every upstream. A mirrored marker must not be
// echoed to the tenant's own server either.
func TestTheLoopGuardAppliesToEveryUpstream(t *testing.T) {
	hosted := newCapturedOTS(t)
	own := newCapturedOTS(t)
	db := storeWithDevice(t, "", "", "")
	_, b := forwarderTo(t, db, hosted.addr, own.addr)

	topic := hubmqtt.TopicPositionFor("t1", "meshsat-device-300434")
	if n := b.deliver(topic, positionPayload(t, "meshsat-device-300434", 52.1, 4.5)); n == 0 {
		t.Fatalf("nothing was subscribed to %s", topic)
	}
	if got := hosted.waitFor(t, 1); len(got) != 0 {
		t.Errorf("the Hub's own marker was echoed to the hosted instance: %v", got)
	}
	if got := own.waitFor(t, 1); len(got) != 0 {
		t.Errorf("the Hub's own marker was echoed to the tenant's own server: %v", got)
	}
}
