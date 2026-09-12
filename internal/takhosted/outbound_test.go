package takhosted

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
	"github.com/meshsat/meshsat-hub/internal/takfront"
)

// --- a bus that lets a test publish as the Hub would ------------------------

type fakeBus struct {
	mu   sync.Mutex
	subs map[string][]bus.MessageHandler
}

func newFakeBus() *fakeBus { return &fakeBus{subs: map[string][]bus.MessageHandler{}} }

func (b *fakeBus) Connect() error                            { return nil }
func (b *fakeBus) IsConnected() bool                         { return true }
func (b *fakeBus) Disconnect()                               {}
func (b *fakeBus) Publish(string, byte, bool, []byte) error  { return nil }
func (b *fakeBus) PublishJSON(string, byte, bool, any) error { return nil }

func (b *fakeBus) Subscribe(filter string, _ byte, h bus.MessageHandler) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[filter] = append(b.subs[filter], h)
	return nil
}

func (b *fakeBus) QueueSubscribe(filter string, q byte, _ string, h bus.MessageHandler) error {
	return b.Subscribe(filter, q, h)
}

// deliver sends a payload to every handler whose filter matches, the way a broker
// would. Only the wildcard shapes this package subscribes to are handled.
func (b *fakeBus) deliver(topic string, payload []byte) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for filter, hs := range b.subs {
		if !filterMatches(filter, topic) {
			continue
		}
		for _, h := range hs {
			h(topic, payload)
			n++
		}
	}
	return n
}

// filterMatches is MQTT single-level wildcard matching, enough for these filters.
func filterMatches(filter, topic string) bool {
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

// --- a TLS server standing in for a tenant's OpenTAKServer ------------------

type capturedOTS struct {
	mu    sync.Mutex
	lines []string
	addr  string
}

func newCapturedOTS(t *testing.T) *capturedOTS {
	t.Helper()
	c := &capturedOTS{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	c.addr = ln.Addr().String()
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				sc := bufio.NewScanner(conn)
				for sc.Scan() {
					c.mu.Lock()
					c.lines = append(c.lines, sc.Text())
					c.mu.Unlock()
				}
			}()
		}
	}()
	return c
}

func (c *capturedOTS) got() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.lines...)
}

func (c *capturedOTS) waitFor(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := c.got(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return c.got()
}

// newForwarderOn wires a Forwarder at a captured upstream, with a plain TCP dial
// standing in for takfront.DialTenant (the TLS policy is tested in takfront).
func newForwarderOn(t *testing.T, ots *capturedOTS, s store.Store) (*Forwarder, *fakeBus) {
	t.Helper()
	b := newFakeBus()
	tenant := &takfront.Tenant{TenantID: "t1", Label: "abcdefghij", Upstream: ots.addr}
	dial := func(ctx context.Context, tn *takfront.Tenant) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", tn.Upstream)
	}
	lookup := func(id string) *takfront.Tenant {
		if id == "t1" {
			return tenant
		}
		return nil
	}
	f := NewForwarder(b, s, dial, lookup, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go f.Run(ctx)
	// Let Run install its subscriptions.
	time.Sleep(50 * time.Millisecond)
	return f, b
}

func positionPayload(t *testing.T, imei string, lat, lon float64) []byte {
	t.Helper()
	raw, err := json.Marshal(store.Position{
		DeviceIMEI: imei, Lat: lat, Lon: lon, Source: "gps", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("marshal position: %v", err)
	}
	return raw
}

func storeWithDevice(t *testing.T, imei, label, typ string) store.Store {
	t.Helper()
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.CreateTenant(ctx, &store.Tenant{
		ID: "t1", Slug: "t1", Name: "t1", Status: store.TenantActive,
	}); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if imei != "" {
		if err := db.CreateDevice(ctx, "t1", &store.Device{IMEI: imei, Label: label, Type: typ}); err != nil {
			t.Fatalf("device: %v", err)
		}
	}
	return db
}

// mustStillBeWired delivers a plain forwardable position and requires it to reach
// the capture server. Every assertion that something is NOT forwarded needs this
// beside it: on its own, such an assertion passes just as well when nothing is
// subscribed at all, and that is not hypothetical here. The first version of
// Forwarder.Run built its filters with hubmqtt.TopicPosition("+"), which encodes
// the wildcard into "meshsat/%2B/position" and therefore matches nothing; all
// three guard tests below passed green while no position could reach any TAK
// server at all.
//
// Call it only where no event has arrived yet, which is exactly what each guard
// test asserts immediately above.
func mustStillBeWired(t *testing.T, ots *capturedOTS, b *fakeBus) {
	t.Helper()
	const control = "999111" // unknown to the store, so isArtefact lets it through
	topic := hubmqtt.TopicPositionFor("t1", control)
	if n := b.deliver(topic, positionPayload(t, control, 52.2, 4.6)); n == 0 {
		t.Fatalf("nothing is subscribed to %s, so the assertion above proved nothing", topic)
	}
	got := ots.waitFor(t, 1)
	if len(got) != 1 {
		t.Fatalf("the positive control did not reach the TAK server (%d events, want 1): "+
			"the forwarder is not wired, so the assertion above proved nothing", len(got))
	}
	if !strings.Contains(got[0], control) {
		t.Errorf("the control event does not carry the control device id:\n%s", got[0])
	}
}

// A tenant's own device position reaches that tenant's TAK server as CoT.
func TestADevicePositionReachesTheTenantsTAKServer(t *testing.T) {
	ots := newCapturedOTS(t)
	db := storeWithDevice(t, "300434", "FIELD-KIT-1", "rockblock")
	_, b := newForwarderOn(t, ots, db)

	topic := hubmqtt.TopicPositionFor("t1", "300434")
	if n := b.deliver(topic, positionPayload(t, "300434", 52.1, 4.5)); n == 0 {
		t.Fatalf("nothing was subscribed to %s", topic)
	}

	lines := ots.waitFor(t, 1)
	if len(lines) == 0 {
		t.Fatal("no CoT reached the tenant's server")
	}
	ev := lines[0]
	for _, want := range []string{"<event", "meshsat-device-300434", "FIELD-KIT-1", "52.1", "4.5"} {
		if !strings.Contains(ev, want) {
			t.Errorf("CoT is missing %q:\n%s", want, ev)
		}
	}
}

// The loop guard, which is the whole reason this file has tests.
//
// The OTS poller mirrors TAK markers back into the devices table as type "tak",
// and those markers include the ones this forwarder just sent. Forwarding them
// again builds a cycle that grows every round. Two independent guards stop it, and
// each is checked separately so neither can quietly stop working.
func TestTheLoopGuardStopsTheForwarderEchoingItsOwnMarkers(t *testing.T) {
	t.Run("a UID carrying the Hub's own prefix is never forwarded", func(t *testing.T) {
		ots := newCapturedOTS(t)
		db := storeWithDevice(t, "", "", "")
		_, b := newForwarderOn(t, ots, db)

		// Exactly what this forwarder publishes as a marker UID.
		topic := hubmqtt.TopicPositionFor("t1", "meshsat-device-300434")
		if n := b.deliver(topic, positionPayload(t, "meshsat-device-300434", 52.1, 4.5)); n == 0 {
			t.Fatalf("nothing is subscribed to %s, so the loop guard is not what stops it", topic)
		}

		if got := ots.waitFor(t, 1); len(got) != 0 {
			t.Errorf("the Hub's own marker was forwarded back, which is the loop:\n%v", got)
		}
		mustStillBeWired(t, ots, b)
	})

	t.Run("a device row an integration created is never forwarded", func(t *testing.T) {
		ots := newCapturedOTS(t)
		// type "tak" is store.ArtefactDeviceTypes: a marker the poller mirrored.
		db := storeWithDevice(t, "ATAK-UID-0001", "somebody's phone", "tak")
		_, b := newForwarderOn(t, ots, db)

		topic := hubmqtt.TopicPositionFor("t1", "ATAK-UID-0001")
		if n := b.deliver(topic, positionPayload(t, "ATAK-UID-0001", 52.1, 4.5)); n == 0 {
			t.Fatalf("nothing is subscribed to %s, so the loop guard is not what stops it", topic)
		}

		if got := ots.waitFor(t, 1); len(got) != 0 {
			t.Errorf("a mirrored TAK marker was forwarded back, which is the loop:\n%v", got)
		}
		mustStillBeWired(t, ots, b)
	})
}

// A tenant with no TAK server is not an error. Most tenants will never turn TAK
// on, and a warning per position would drown the log.
func TestATenantWithNoTAKServerIsSilentlySkipped(t *testing.T) {
	ots := newCapturedOTS(t)
	db := storeWithDevice(t, "300434", "KIT", "rockblock")
	_, b := newForwarderOn(t, ots, db)

	// t2 has no tenant in the lookup.
	topic := hubmqtt.TopicPositionFor("t2", "300434")
	if n := b.deliver(topic, positionPayload(t, "300434", 52.1, 4.5)); n == 0 {
		t.Fatalf("nothing is subscribed to %s, so the absence below is a filter miss "+
			"rather than the skip this test is about", topic)
	}
	if got := ots.waitFor(t, 1); len(got) != 0 {
		t.Errorf("a position for a tenant with no TAK server was forwarded:\n%v", got)
	}
	mustStillBeWired(t, ots, b)
}

// Null Island is not a position: devices report 0,0 when they have no fix, and
// putting that on a map is worse than leaving the device absent.
func TestAZeroFixIsNotForwarded(t *testing.T) {
	ots := newCapturedOTS(t)
	db := storeWithDevice(t, "300434", "KIT", "rockblock")
	_, b := newForwarderOn(t, ots, db)

	topic := hubmqtt.TopicPositionFor("t1", "300434")
	if n := b.deliver(topic, positionPayload(t, "300434", 0, 0)); n == 0 {
		t.Fatalf("nothing is subscribed to %s, so the zero-fix guard is not what stops it", topic)
	}
	if got := ots.waitFor(t, 1); len(got) != 0 {
		t.Errorf("a 0,0 fix was forwarded:\n%v", got)
	}
	mustStillBeWired(t, ots, b)
}

// One connection is reused across positions: every upstream connection forks a
// process in OpenTAKServer, so a kit reporting every minute must not fork one each
// time.
func TestTheUpstreamConnectionIsReusedAcrossPositions(t *testing.T) {
	ots := newCapturedOTS(t)
	db := storeWithDevice(t, "300434", "KIT", "rockblock")
	f, b := newForwarderOn(t, ots, db)

	for i := 0; i < 3; i++ {
		b.deliver(hubmqtt.TopicPositionFor("t1", "300434"), positionPayload(t, "300434", 52.0+float64(i)/10, 4.5))
	}
	if got := ots.waitFor(t, 3); len(got) < 3 {
		t.Fatalf("only %d of 3 events arrived: %v", len(got), got)
	}
	f.mu.Lock()
	open := len(f.conns)
	f.mu.Unlock()
	if open != 1 {
		t.Errorf("%d upstream connections open for one tenant, want 1", open)
	}
}

// The legacy topic shape must work too. meshsat/+/position does not match
// meshsat/{tenant}/{device}/position, so subscribing to only one shape leaves
// either the default tenant or every other tenant silent.
func TestBothTopicShapesAreForwarded(t *testing.T) {
	ots := newCapturedOTS(t)
	db := storeWithDevice(t, "300434", "KIT", "rockblock")
	_, b := newForwarderOn(t, ots, db)

	// The legacy, tenant-less shape resolves to the default tenant, which this
	// lookup does not serve -- so assert the SUBSCRIPTION exists rather than the
	// delivery, which is what DualFilters is responsible for.
	legacy := hubmqtt.TopicPosition("300434")
	if n := b.deliver(legacy, positionPayload(t, "300434", 52.1, 4.5)); n == 0 {
		t.Errorf("nothing is subscribed to the legacy shape %q; DualFilters was not used", legacy)
	}
}

// A dead upstream must not wedge the forwarder: it drops the connection and the
// next position redials.
func TestADeadUpstreamIsDroppedAndRedialled(t *testing.T) {
	ots := newCapturedOTS(t)
	db := storeWithDevice(t, "300434", "KIT", "rockblock")
	f, b := newForwarderOn(t, ots, db)

	b.deliver(hubmqtt.TopicPositionFor("t1", "300434"), positionPayload(t, "300434", 52.1, 4.5))
	ots.waitFor(t, 1)

	// Close the held connection behind the forwarder's back.
	f.mu.Lock()
	for _, c := range f.conns {
		_ = c.conn.Close()
	}
	f.mu.Unlock()

	b.deliver(hubmqtt.TopicPositionFor("t1", "300434"), positionPayload(t, "300434", 52.2, 4.6))
	if got := ots.waitFor(t, 2); len(got) < 2 {
		t.Errorf("after the upstream died the forwarder did not redial: %d events", len(got))
	}
}

// PublishEmpty must CLEAR the tenant map, not merely replace the directory. A
// purged tenant left dialable would keep receiving another customer's traffic.
func TestPublishEmptyClearsTheDialableTenants(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK aaaaaaaaaa CA")
	api := newFakeAPI(ca)
	api.autoIssue = true
	api.setInstances(readyInstance("t1", "aaaaaaaaaa", ca.pemStr))

	srv := httptest.NewServer(api.handler(t))
	t.Cleanup(srv.Close)
	cl := NewClientWith(srv.Client(), srv.URL, "meshsat-tak")
	r := NewDirectoryRefresher(cl, NewIdentityKeeper(cl, nil), func(*takfront.Directory) {}, nil)

	prime(t, r)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if r.TenantByID("t1") == nil {
		t.Fatal("a Ready tenant is not dialable after a refresh")
	}

	if err := r.PublishEmpty(); err != nil {
		t.Fatalf("PublishEmpty: %v", err)
	}
	if got := r.TenantByID("t1"); got != nil {
		t.Error("the tenant is still dialable after the directory was emptied; " +
			"a purged tenant would keep receiving traffic")
	}
}

// TenantByID hands back a COPY, so a caller cannot mutate the refresher's state.
func TestTenantByIDReturnsACopy(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK aaaaaaaaaa CA")
	api := newFakeAPI(ca)
	api.autoIssue = true
	api.setInstances(readyInstance("t1", "aaaaaaaaaa", ca.pemStr))

	srv := httptest.NewServer(api.handler(t))
	t.Cleanup(srv.Close)
	cl := NewClientWith(srv.Client(), srv.URL, "meshsat-tak")
	r := NewDirectoryRefresher(cl, NewIdentityKeeper(cl, nil), func(*takfront.Directory) {}, nil)
	prime(t, r)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	first := r.TenantByID("t1")
	if first == nil {
		t.Fatal("not dialable")
	}
	first.Upstream = "somewhere-else:9999"

	second := r.TenantByID("t1")
	if second.Upstream == "somewhere-else:9999" {
		t.Error("TenantByID handed out a pointer into its own state; a caller mutated it")
	}
}

var _ bus.MessageBus = (*fakeBus)(nil)
