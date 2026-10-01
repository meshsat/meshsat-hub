package takhosted

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/netguard"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/takfront"
)

// MESHSAT-1460: the forwarder never blocks the bus, never forwards twice after a
// change of leader, and cannot be pointed at an address inside the cluster.

// --- helpers the older tests use too -----------------------------------------

// openConns counts the upstream connections the current run holds open.
func openConns(f *Forwarder) int {
	r := f.cur.Load()
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, l := range r.links {
		l.mu.Lock()
		if l.conn != nil {
			n++
		}
		l.mu.Unlock()
	}
	return n
}

// closeConnsBehindItsBack closes every held connection without telling the link,
// which is what a server going away looks like from here.
func closeConnsBehindItsBack(f *Forwarder) {
	r := f.cur.Load()
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.links {
		l.mu.Lock()
		if l.conn != nil {
			_ = l.conn.Close()
		}
		l.mu.Unlock()
	}
}

// syncBuffer is a log sink a test may read while the forwarder writes. The
// forwarder logs from its own goroutines now, so a bare bytes.Buffer is a data
// race between the link and the assertion.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

// handlerCount is how many handlers the bus holds across every filter.
func (b *fakeBus) handlerCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, hs := range b.subs {
		n += len(hs)
	}
	return n
}

// plainDial stands in for the TLS policy, which is tested in takfront.
func plainDial(ctx context.Context, tn *takfront.Tenant) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", tn.Upstream)
}

// oneTenant is an upstream lookup serving t1 only.
func oneTenant(addr string) func(context.Context, string) []*takfront.Tenant {
	tn := &takfront.Tenant{TenantID: "t1", Label: "abcdefghij", Upstream: addr}
	return func(_ context.Context, id string) []*takfront.Tenant {
		if id == "t1" {
			return []*takfront.Tenant{tn}
		}
		return nil
	}
}

// startRun runs the forwarder as one tenure and returns how to end it. The
// returned function cancels and waits for Run to have returned, which is what
// losing the lease does.
func startRun(t *testing.T, f *Forwarder) (lose func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.Run(ctx)
	}()
	// Let Run publish its run and install its filters.
	deadline := time.Now().Add(2 * time.Second)
	for f.cur.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.cur.Load() == nil {
		t.Fatal("Run never started")
	}
	lost := false
	lose = func() {
		if lost {
			return
		}
		lost = true
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return after its context was cancelled")
		}
	}
	t.Cleanup(lose)
	return lose
}

func deliverPosition(t *testing.T, b *fakeBus, tenant, imei string, lat float64) {
	t.Helper()
	topic := hubmqtt.TopicPositionFor(tenant, imei)
	if n := b.deliver(topic, positionPayload(t, imei, lat, 4.5)); n == 0 {
		t.Fatalf("nothing is subscribed to %s", topic)
	}
}

// --- subscribe once, gated on the run ---------------------------------------

// The bus has no unsubscribe. Subscribing inside Run left one more handler
// behind on every acquisition of the lease.
func TestRunSubscribesOnce(t *testing.T) {
	ots := newCapturedOTS(t)
	b := newFakeBus()
	f := NewForwarder(b, storeWithDevice(t, "300434", "KIT", "rockblock"), plainDial, oneTenant(ots.addr), nil)

	lose := startRun(t, f)
	first := b.handlerCount()
	if first == 0 {
		t.Fatal("the first run installed no handlers")
	}
	lose()

	for i := 0; i < 3; i++ {
		lose = startRun(t, f)
		lose()
	}
	if got := b.handlerCount(); got != first {
		t.Errorf("%d handlers after four tenures, %d after one: each acquisition of the lease "+
			"left handlers behind, and each of them forwards", got, first)
	}
}

// A replica that has lost the lease must not try to forward, and must not log a
// failure for every position it can no longer forward (MESHSAT-1457: 103 such
// warnings in four hours on the pod that had stopped being the leader).
func TestAfterTheLeaseIsLostHandlersAreSilent(t *testing.T) {
	ots := newCapturedOTS(t)
	b := newFakeBus()
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f := NewForwarder(b, storeWithDevice(t, "300434", "KIT", "rockblock"), plainDial, oneTenant(ots.addr), log)

	lose := startRun(t, f)
	deliverPosition(t, b, "t1", "300434", 52.1)
	if got := ots.waitFor(t, 1); len(got) != 1 {
		t.Fatalf("the control position did not arrive while leader: %d events", len(got))
	}
	lose()
	logs.Reset()
	if f.cur.Load() != nil {
		t.Fatal("the run is still published after the lease was lost: the handlers would keep " +
			"queueing for a dispatcher that has gone, and count every message as dropped")
	}

	// The handlers are still on the bus, by design. They must do nothing.
	deliverPosition(t, b, "t1", "300434", 52.2)
	time.Sleep(300 * time.Millisecond)

	if got := ots.got(); len(got) != 1 {
		t.Errorf("a replica that had lost the lease forwarded anyway: %d events, want 1", len(got))
	}
	if out := logs.String(); strings.Contains(out, "failed") || strings.Contains(out, "canceled") {
		t.Errorf("a replica that had lost the lease logged about a position that was never its "+
			"to forward:\n%s", out)
	}
}

// The worse half of the same defect: lose the lease, win it back, and the stale
// handler wrote every position a second time through the new run's connection.
func TestReacquiringTheLeaseDoesNotForwardTwice(t *testing.T) {
	ots := newCapturedOTS(t)
	b := newFakeBus()
	f := NewForwarder(b, storeWithDevice(t, "300434", "KIT", "rockblock"), plainDial, oneTenant(ots.addr), nil)

	lose := startRun(t, f)
	lose()
	startRun(t, f)

	deliverPosition(t, b, "t1", "300434", 52.1)
	if got := ots.waitFor(t, 1); len(got) != 1 {
		t.Fatalf("the position did not arrive after the lease was won back: %d events", len(got))
	}
	// Long enough for a second copy to have arrived if one was sent.
	time.Sleep(300 * time.Millisecond)
	if got := ots.got(); len(got) != 1 {
		t.Errorf("one position was forwarded %d times after the lease was lost and won back; "+
			"every device would be drawn twice", len(got))
	}
}

// --- nothing blocks the bus -------------------------------------------------

// The bus delivers in order on one goroutine. A handler that dials makes one
// tenant's unreachable TAK server everybody's outage, SOS detection included.
func TestASlowUpstreamNeverBlocksTheBusHandler(t *testing.T) {
	healthy := newCapturedOTS(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	stuck := &takfront.Tenant{TenantID: "stuck", Label: externalLabel, Upstream: "203.0.113.1:8089"}
	fine := &takfront.Tenant{TenantID: store.DefaultTenantID, Label: "abcdefghij", Upstream: healthy.addr}
	dial := func(ctx context.Context, tn *takfront.Tenant) (net.Conn, error) {
		if tn == stuck {
			// A server that never answers: the dial hangs until the test ends.
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil, errors.New("never answered")
		}
		return plainDial(ctx, tn)
	}
	ups := func(_ context.Context, id string) []*takfront.Tenant {
		switch id {
		case "stuck":
			return []*takfront.Tenant{stuck}
		case store.DefaultTenantID:
			return []*takfront.Tenant{fine}
		}
		return nil
	}
	b := newFakeBus()
	f := NewForwarder(b, storeWithDevice(t, "", "", ""), dial, ups, nil)
	startRun(t, f)

	began := time.Now()
	for i := 0; i < 50; i++ {
		deliverPosition(t, b, "stuck", "300434", 52.0+float64(i)/100)
	}
	deliverPosition(t, b, store.DefaultTenantID, "300999", 52.5)
	if took := time.Since(began); took > time.Second {
		t.Fatalf("51 bus deliveries took %v with one tenant's TAK server hanging: the handler "+
			"is waiting on the network, so that server stalls the whole bus", took)
	}

	// And the tenant whose server works is not made to wait for the one whose
	// server does not.
	if got := healthy.waitFor(t, 1); len(got) != 1 {
		t.Errorf("a healthy tenant's position did not arrive while another tenant's server "+
			"hung: %d events, want 1", len(got))
	}
}

// A dead server is not dialled once per position. The events are still counted
// as lost, so the alert on that counter keeps its meaning.
func TestADeadUpstreamIsNotRedialledForEveryPosition(t *testing.T) {
	var dials atomic.Int32
	dead := deadAddress(t)
	dial := func(ctx context.Context, tn *takfront.Tenant) (net.Conn, error) {
		dials.Add(1)
		return plainDial(ctx, tn)
	}
	b := newFakeBus()
	f := NewForwarder(b, storeWithDevice(t, "300434", "KIT", "rockblock"), dial, oneTenant(dead), nil)
	startRun(t, f)

	before := counterValue(t, kindHosted, reasonDial)
	const n = 5
	for i := 0; i < n; i++ {
		deliverPosition(t, b, "t1", "300434", 52.0+float64(i)/100)
	}

	deadline := time.Now().Add(3 * time.Second)
	for counterValue(t, kindHosted, reasonDial)-before < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if lost := counterValue(t, kindHosted, reasonDial) - before; lost != n {
		t.Errorf("%v of %d lost events were counted; the alert on this counter would under-report "+
			"a server that is down", lost, n)
	}
	if got := dials.Load(); got != 1 {
		t.Errorf("a dead server was dialled %d times for %d positions inside its backoff, want 1", got, n)
	}
}

// The wait grows, is bounded at both ends, and is never zero.
func TestTheRedialWaitGrowsAndIsBounded(t *testing.T) {
	now := time.Unix(1_760_000_000, 123_456_789)
	prev := time.Duration(0)
	for fails := 1; fails <= 20; fails++ {
		d := backoffFor(fails, now)
		if d <= 0 {
			t.Fatalf("after %d failures the wait is %v: a link would redial in a tight loop", fails, d)
		}
		if d > linkBackoffMax {
			t.Fatalf("after %d failures the wait is %v, over the %v ceiling", fails, d, linkBackoffMax)
		}
		if fails <= 8 && d <= prev {
			t.Errorf("the wait did not grow from failure %d to %d: %v then %v", fails-1, fails, prev, d)
		}
		prev = d
	}
	if first := backoffFor(1, now); first > linkBackoffMin {
		t.Errorf("the first wait is %v, over %v: one blip would cost a tenant its next position too",
			first, linkBackoffMin)
	}
}

// --- eviction ---------------------------------------------------------------

// A purged or closed tenant must not keep a connection to its TAK server, and
// dropping it must not touch anybody else's.
func TestForgetTenantClosesOnlyThatTenantsLinks(t *testing.T) {
	victim := newCapturedOTS(t)
	other := newCapturedOTS(t)
	b := forwarderForTenantsF(t, storeWithDevice(t, "", "", ""), map[string]*capturedOTS{
		store.DefaultTenantID: victim,
		"t-other":             other,
	})
	f := b.f

	deliverPosition(t, b.fakeBus, store.DefaultTenantID, "300001", 52.1)
	deliverPosition(t, b.fakeBus, "t-other", "300002", 52.2)
	victim.waitFor(t, 1)
	other.waitFor(t, 1)
	if got := openConns(f); got != 2 {
		t.Fatalf("%d connections open for two tenants, want 2", got)
	}

	f.ForgetTenant("t-other")

	deadline := time.Now().Add(2 * time.Second)
	for openConns(f) != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := openConns(f); got != 1 {
		t.Fatalf("%d connections open after one tenant was forgotten, want 1", got)
	}

	// The tenant that was not forgotten still forwards on the connection it had.
	deliverPosition(t, b.fakeBus, store.DefaultTenantID, "300001", 52.3)
	if got := victim.waitFor(t, 2); len(got) != 2 {
		t.Errorf("forgetting one tenant stopped another's forwarding: %d events, want 2", len(got))
	}
	// And a forgotten tenant that comes back is simply dialled again.
	deliverPosition(t, b.fakeBus, "t-other", "300002", 52.4)
	if got := other.waitFor(t, 2); len(got) != 2 {
		t.Errorf("a forgotten tenant's next position did not arrive: %d events, want 2", len(got))
	}
}

// busAndForwarder is forwarderForTenants with the Forwarder kept.
type busAndForwarder struct {
	*fakeBus
	f *Forwarder
}

func forwarderForTenantsF(t *testing.T, s store.Store, ots map[string]*capturedOTS) busAndForwarder {
	t.Helper()
	tenants := make(map[string]*takfront.Tenant, len(ots))
	for id, o := range ots {
		tenants[id] = &takfront.Tenant{TenantID: id, Label: "abcdefghij", Upstream: o.addr}
	}
	ups := func(_ context.Context, id string) []*takfront.Tenant {
		if tn, ok := tenants[id]; ok {
			return []*takfront.Tenant{tn}
		}
		return nil
	}
	b := newFakeBus()
	f := NewForwarder(b, s, plainDial, ups, nil)
	startRun(t, f)
	return busAndForwarder{fakeBus: b, f: f}
}

// --- a tenant's own server may not be an internal address --------------------

// The address of a tenant's own TAK server is whatever a customer typed. With no
// guard on the dial, the Hub opens a TLS connection to anything in the cluster
// with its own network identity.
func TestATenantsOwnServerOnAnInternalAddressIsNotDialled(t *testing.T) {
	var accepted atomic.Int32
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = c.Close()
		}
	}()
	host, port, _ := net.SplitHostPort(ln.Addr().String())

	v := goodTAKValues(t)
	v[takFieldHost], v[takFieldPort] = host, port
	tn, why := buildExternalTenant("t1", v)
	if tn == nil {
		t.Fatalf("the settings did not build: %s", why)
	}
	if tn.DialControl == nil {
		t.Fatal("a tenant's own TAK server carries no dial guard: the Hub would connect to " +
			"whatever address the customer typed")
	}

	_, err = takfront.DialUpstream(context.Background(), tn, 2*time.Second)
	if err == nil {
		t.Fatal("the Hub connected to a loopback address a tenant configured as their TAK server")
	}
	if !errors.Is(err, netguard.ErrUnsafe) {
		t.Errorf("the dial failed, but not because the guard refused it: %v", err)
	}
	if got := accepted.Load(); got != 0 {
		t.Errorf("the listener saw %d connections: the guard ran after the connect, not before", got)
	}

	// The control: without the guard the same dial reaches the listener, so the
	// guard is what stopped it and not, say, a wrong port.
	tn.DialControl = nil
	if c, err := takfront.DialUpstream(context.Background(), tn, 2*time.Second); err == nil {
		_ = c.Close()
	}
	deadline := time.Now().Add(2 * time.Second)
	for accepted.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if accepted.Load() == 0 {
		t.Error("the unguarded dial never reached the listener either, so the assertions above " +
			"proved nothing about the guard")
	}
}

// A hosted instance is an in-cluster name by design and must stay dialable: the
// guard belongs to addresses a customer typed, not to ones the operator made.
func TestAHostedInstanceCarriesNoDialGuard(t *testing.T) {
	ca := newTestCA(t, "MeshSat TAK aaaaaaaaaa CA")
	api := newFakeAPI(ca)
	api.autoIssue = true
	api.setInstances(readyInstance("t1", "aaaaaaaaaa", ca.pemStr))

	srv := httptest.NewServer(api.handler(t))
	t.Cleanup(srv.Close)
	cl := NewClientWith(srv.Client(), srv.URL, "meshsat-tak")
	r := NewDirectoryRefresher(cl, NewIdentityKeeper(cl, "replica-one", nil), func(*takfront.Directory) {}, nil)
	prime(t, r)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	tn := r.TenantByID("t1")
	if tn == nil {
		t.Fatal("the hosted instance is not dialable, so this proves nothing")
	}
	if tn.DialControl != nil {
		t.Error("a hosted instance carries the public-address guard; its in-cluster address " +
			"would be refused and every hosted tenant would lose its map")
	}
}

// A server that is not keeping up loses its OLDEST waiting event, not the newest:
// a position is worth only its freshness, so the one to keep is the latest.
func TestAFullQueueLosesItsOldestEvent(t *testing.T) {
	release := make(chan struct{})
	ots := newCapturedOTS(t)
	dial := func(ctx context.Context, tn *takfront.Tenant) (net.Conn, error) {
		// Hold the link in its first dial until every event has been queued.
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return plainDial(ctx, tn)
	}
	b := newFakeBus()
	f := NewForwarder(b, storeWithDevice(t, "", "", ""), dial, oneTenant(ots.addr), nil)
	startRun(t, f)

	before := counterValue(t, kindHosted, reasonQueue)
	// One event is taken by the link and held in the dial; linkQueue more fill
	// the queue; the rest displace the oldest of those. Each event is a different
	// device, so the CoT says which one it was.
	const extra = 10
	total := 1 + linkQueue + extra
	imei := func(i int) string { return fmt.Sprintf("3004%05d", i) }
	for i := 0; i < total; i++ {
		deliverPosition(t, b, "t1", imei(i), 52.1)
		if i == 0 {
			// Let the link take the first event and block in its dial, so the
			// arithmetic above holds.
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				r := f.cur.Load()
				r.mu.Lock()
				l := r.links[upstreamKey("t1", &takfront.Tenant{Upstream: ots.addr})]
				taken := l != nil && len(l.q) == 0
				r.mu.Unlock()
				if taken {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
	}
	// The dispatcher is asynchronous: wait until it has counted every displaced
	// event before letting the link drain.
	deadline := time.Now().Add(5 * time.Second)
	for counterValue(t, kindHosted, reasonQueue)-before < extra && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if lost := counterValue(t, kindHosted, reasonQueue) - before; lost != extra {
		t.Fatalf("%v events were counted as displaced, want %d", lost, extra)
	}
	close(release)

	got := ots.waitFor(t, 1+linkQueue)
	if len(got) != 1+linkQueue {
		t.Fatalf("%d events arrived, want %d", len(got), 1+linkQueue)
	}
	all := strings.Join(got, "\n")
	if !strings.Contains(got[len(got)-1], imei(total-1)) {
		t.Errorf("the newest position is not the last one written: a full queue dropped the "+
			"fresh event and kept a stale one.\nlast written:\n%s", got[len(got)-1])
	}
	// The displaced ones are the oldest that were WAITING: 1 to extra. Event 0 had
	// already been taken by the link.
	for i := 1; i <= extra; i++ {
		if strings.Contains(all, imei(i)) {
			t.Errorf("event %d was written although %d newer ones displaced it", i, extra)
		}
	}
	if !strings.Contains(all, imei(extra+1)) {
		t.Errorf("event %d, the oldest that should have survived, is missing", extra+1)
	}
}
