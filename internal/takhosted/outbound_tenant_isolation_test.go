package takhosted

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/takfront"
)

// MESHSAT-1032. The original defect was a CoT gateway that subscribed through
// DualFilters and forwarded EVERY tenant's positions, SOS and MO text to one platform
// OpenTAKServer with no tenant filter — every ATAK client on that server saw every
// customer's markers. The rearchitecture to per-tenant hosted TAK removed the mechanism:
// onPosition parses the tenant off the topic and asks upstreams() for THAT tenant's
// servers only.
//
// Nothing held that, though. The isolation is the single `f.upstreams(ctx, tenantID)`
// call in onPosition; passing anything else there — a captured variable, a default, a
// merged list — puts the leak straight back, and every existing test in this package uses
// exactly one tenant with one upstream, so none of them would notice. These tests exist to
// notice.
//
// ⚠ The victim is store.DefaultTenantID, deliberately, per the house rule for tenant
// isolation tests: this class of bug has no tenant dimension at all, so a regression
// collapses onto the default tenant and a test using two non-default tenants passes by
// accident.

// forwarderForTenants wires one Forwarder over several tenants, each with its own captured
// server, so "did it go to the right one" is answerable.
func forwarderForTenants(t *testing.T, s store.Store, ots map[string]*capturedOTS) *fakeBus {
	t.Helper()
	b := newFakeBus()

	tenants := make(map[string]*takfront.Tenant, len(ots))
	for id, o := range ots {
		tenants[id] = &takfront.Tenant{TenantID: id, Label: "abcdefghij", Upstream: o.addr}
	}
	dial := func(ctx context.Context, tn *takfront.Tenant) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", tn.Upstream)
	}
	ups := func(_ context.Context, id string) []*takfront.Tenant {
		if tn, ok := tenants[id]; ok {
			return []*takfront.Tenant{tn}
		}
		return nil
	}
	f := NewForwarder(b, s, dial, ups, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go f.Run(ctx)
	time.Sleep(50 * time.Millisecond) // let Run install its subscriptions
	return b
}

// quiet asserts a server received nothing, after giving a leak time to arrive. A bare
// len(got)==0 immediately after publishing would pass even with the leak present, because
// the forwarder dials and writes asynchronously.
//
// The window is short where the caller has ALREADY confirmed the positive delivery: both
// sends happen inside the same onPosition call, so if the right server has the event, a
// leak to the wrong one has already been attempted. Where there is no positive side to
// wait on, the caller passes a longer window because nothing else proves the handler ran.
func quiet(t *testing.T, c *capturedOTS, who string, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if got := c.got(); len(got) > 0 {
			t.Fatalf("CROSS-TENANT LEAK: %s received %d CoT event(s) it must never see:\n%s",
				who, len(got), strings.Join(got, "\n"))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestOneTenantsPositionNeverReachesAnotherTenantsTAKServer(t *testing.T) {
	victim := newCapturedOTS(t)   // store.DefaultTenantID — the platform's own server
	customer := newCapturedOTS(t) // a paying tenant

	db := storeWithDevice(t, "300434", "FIELD-KIT-1", "rockblock")
	b := forwarderForTenants(t, db, map[string]*capturedOTS{
		store.DefaultTenantID: victim,
		"t-customer":          customer,
	})

	// A customer device reports its position on the customer's topic shape.
	topic := hubmqtt.TopicPositionFor("t-customer", "300434")
	if n := b.deliver(topic, positionPayload(t, "300434", 52.1, 4.5)); n == 0 {
		t.Fatalf("nothing was subscribed to %s — the test proves nothing", topic)
	}

	// It must reach the customer's own server...
	if lines := customer.waitFor(t, 1); len(lines) == 0 {
		t.Fatal("the customer's own TAK server received nothing — the forwarder is not " +
			"working at all, so the isolation assertion below would be vacuous")
	}
	// ...and the platform's server must never see it.
	quiet(t, victim, "the PLATFORM (default tenant) TAK server", 600*time.Millisecond)
}

func TestThePlatformsPositionNeverReachesACustomersTAKServer(t *testing.T) {
	platform := newCapturedOTS(t)
	customer := newCapturedOTS(t)

	db := storeWithDevice(t, "300434", "FIELD-KIT-1", "rockblock")
	b := forwarderForTenants(t, db, map[string]*capturedOTS{
		store.DefaultTenantID: platform,
		"t-customer":          customer,
	})

	// The platform tenant uses the legacy three-segment shape; both shapes are subscribed.
	topic := hubmqtt.TopicPositionFor(store.DefaultTenantID, "300434")
	if n := b.deliver(topic, positionPayload(t, "300434", 52.1, 4.5)); n == 0 {
		t.Fatalf("nothing was subscribed to %s — the test proves nothing", topic)
	}

	if lines := platform.waitFor(t, 1); len(lines) == 0 {
		t.Fatal("the platform's own TAK server received nothing — isolation assertion would be vacuous")
	}
	quiet(t, customer, "the CUSTOMER TAK server", 600*time.Millisecond)
}

// A tenant that has not configured TAK must not fall back to anyone else's server. This is
// the shape the original bug actually had: no upstream of its own, so it used the one
// global one.
func TestATenantWithNoTAKServerFallsBackToNobody(t *testing.T) {
	platform := newCapturedOTS(t)

	db := storeWithDevice(t, "300434", "FIELD-KIT-1", "rockblock")
	b := forwarderForTenants(t, db, map[string]*capturedOTS{
		store.DefaultTenantID: platform,
	})

	topic := hubmqtt.TopicPositionFor("t-no-tak", "300434")
	if n := b.deliver(topic, positionPayload(t, "300434", 52.1, 4.5)); n == 0 {
		t.Fatalf("nothing was subscribed to %s — the test proves nothing", topic)
	}
	quiet(t, platform, "the PLATFORM TAK server (a tenant with no TAK server of its own)", 2*time.Second)
}
