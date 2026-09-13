package tenancy

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
)

// loopBus is a MessageBus that delivers to subscribers in the same process,
// including across two Evictors, so a test can watch one replica evict because
// another one said to.
type loopBus struct {
	mu       sync.Mutex
	handlers map[string][]bus.MessageHandler
	sent     []string
	failWith error
}

func newLoopBus() *loopBus { return &loopBus{handlers: map[string][]bus.MessageHandler{}} }

func (b *loopBus) Connect() error    { return nil }
func (b *loopBus) IsConnected() bool { return true }
func (b *loopBus) Disconnect()       {}

func (b *loopBus) Publish(topic string, _ byte, _ bool, payload []byte) error {
	b.mu.Lock()
	if b.failWith != nil {
		err := b.failWith
		b.mu.Unlock()
		return err
	}
	b.sent = append(b.sent, topic)
	hs := append([]bus.MessageHandler(nil), b.handlers[topic]...)
	b.mu.Unlock()
	for _, h := range hs {
		h(topic, payload)
	}
	return nil
}

func (b *loopBus) PublishJSON(topic string, q byte, r bool, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return b.Publish(topic, q, r, raw)
}

func (b *loopBus) Subscribe(topic string, _ byte, h bus.MessageHandler) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[topic] = append(b.handlers[topic], h)
	return nil
}

func (b *loopBus) QueueSubscribe(topic string, q byte, _ string, h bus.MessageHandler) error {
	return b.Subscribe(topic, q, h)
}

func (b *loopBus) published() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.sent...)
}

type spyCache struct {
	mu     sync.Mutex
	forgot []string
}

func (s *spyCache) ForgetTenant(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgot = append(s.forgot, id)
}

func (s *spyCache) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.forgot...)
}

// The defect this package exists for: five caches documented a Forget as "what
// the purge path calls" and nothing called any of them.
func TestEvictReachesEveryRegisteredCache(t *testing.T) {
	a, b, c := &spyCache{}, &spyCache{}, &spyCache{}
	e := NewEvictor(nil, nil)
	e.Register(a)
	e.Register(b)
	e.Register(c)

	e.Evict("t_gone")

	for i, s := range []*spyCache{a, b, c} {
		if got := s.seen(); len(got) != 1 || got[0] != "t_gone" {
			t.Errorf("cache %d saw %v, want [t_gone]", i, got)
		}
	}
}

// The one that matters most, and the one a single-replica test would miss: the
// purge job is a leader singleton, so evicting only where it runs leaves the
// other replicas holding the tenant.
func TestAnEvictionOnOneReplicaReachesTheOthers(t *testing.T) {
	line := newLoopBus()

	// Replica B is listening.
	other := &spyCache{}
	eB := NewEvictor(line, nil)
	eB.Register(other)
	if err := eB.Subscribe(); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Replica A performs the purge.
	mine := &spyCache{}
	eA := NewEvictor(line, nil)
	eA.Register(mine)
	eA.Evict("t_gone")

	if got := mine.seen(); len(got) != 1 {
		t.Errorf("the evicting replica saw %v, want one eviction", got)
	}
	if got := other.seen(); len(got) != 1 || got[0] != "t_gone" {
		t.Errorf("the OTHER replica saw %v, want [t_gone] -- it is still holding a purged tenant", got)
	}
}

// A bus failure must not be fatal: the rows are already gone, so nothing new
// can resolve to the tenant. What survives is stale cached answers elsewhere.
func TestEvictionStillHappensLocallyWhenTheBusIsDown(t *testing.T) {
	line := newLoopBus()
	line.failWith = errors.New("broker down")
	mine := &spyCache{}
	e := NewEvictor(line, nil)
	e.Register(mine)

	e.Evict("t_gone") // must not panic

	if got := mine.seen(); len(got) != 1 {
		t.Errorf("local eviction saw %v, want it to happen anyway", got)
	}
}

// StatusCache.Forget announces on StatusTopic. If the Evictor called that
// instead of ForgetTenant, every replica would publish a status event on
// receipt of every purge -- N messages for one eviction.
func TestEvictingAStatusCacheDoesNotAnnounceAgain(t *testing.T) {
	line := newLoopBus()
	sc := NewStatusCache(nil, 0).WithBus(line)

	e := NewEvictor(nil, nil) // no bus of its own, so anything published came from the cache
	e.Register(sc)
	e.Evict("t_gone")

	if got := line.published(); len(got) != 0 {
		t.Errorf("evicting published %v; the status cache re-announced an eviction it was "+
			"told about, which multiplies one purge into a message per replica", got)
	}
}

func TestEvictorToleratesNilsAndEmptyIDs(t *testing.T) {
	var nilEvictor *Evictor
	nilEvictor.Evict("t1") // must not panic
	nilEvictor.Register(&spyCache{})

	spy := &spyCache{}
	e := NewEvictor(nil, nil)
	e.Register(nil)
	e.Register(spy)
	e.Evict("")
	if got := spy.seen(); len(got) != 0 {
		t.Errorf("an empty tenant id evicted %v", got)
	}
}

// --- the resolver's tenant-wide eviction ---

// Forget takes an IMEI because it exists for one device. A purge has no list of
// IMEIs to pass -- the device rows are already deleted -- so the resolver has to
// find them itself.
func TestForgetTenantDropsOnlyThatTenantsDevicesAndBridges(t *testing.T) {
	r := NewResolver(&stubStore{
		devices: map[string]string{"dev_gone": "t_gone", "dev_stays": "t_stays"},
		bridges: map[string]string{"br_gone": "t_gone", "br_stays": "t_stays"},
	}, "default", time.Hour)
	ctx := context.Background()

	// Warm every map, including the tenant-exists one.
	r.ForDevice(ctx, "dev_gone")
	r.ForDevice(ctx, "dev_stays")
	r.ForBridge(ctx, "br_gone")
	r.ForBridge(ctx, "br_stays")
	r.tenantExists(ctx, "t_known")

	r.ForgetTenant("t_gone")

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.devices["dev_gone"]; ok {
		t.Error("a purged tenant's device is still cached")
	}
	if _, ok := r.bridges["br_gone"]; ok {
		t.Error("a purged tenant's bridge is still cached")
	}
	if _, ok := r.devices["dev_stays"]; !ok {
		t.Error("another tenant's device was evicted")
	}
	if _, ok := r.bridges["br_stays"]; !ok {
		t.Error("another tenant's bridge was evicted")
	}
}

// Leaving the existence entry would keep Known() answering "yes, that tenant
// exists" for a full TTL after its rows were destroyed -- the window in which an
// unregistered device naming it in a topic is adopted into a tenant that is gone.
func TestForgetTenantDropsTheTenantsOwnExistenceEntry(t *testing.T) {
	r := NewResolver(&stubStore{}, "default", time.Hour)
	ctx := context.Background()
	if !r.tenantExists(ctx, "t_known") {
		t.Fatal("fixture: t_known should be known")
	}

	r.ForgetTenant("t_known")

	r.mu.Lock()
	_, cached := r.tenants["t_known"]
	r.mu.Unlock()
	if cached {
		t.Error("the purged tenant is still cached as existing")
	}
}

// --- the purge job actually calls it ---

func TestAPurgeEvictsTheTenantFromTheCaches(t *testing.T) {
	db, tenantID := purgeFixture(t)
	spy := &spyCache{}
	ev := NewEvictor(nil, nil)
	ev.Register(spy)

	j := NewPurgeJob(db, nil, 0)
	j.SetEvictor(ev)
	j.once(context.Background())

	if got := spy.seen(); len(got) != 1 || got[0] != tenantID {
		t.Errorf("the caches were asked to forget %v, want [%s]; without this the tenant's "+
			"rows are gone and it goes on living in every replica's memory", got, tenantID)
	}
}

// A purge that did not happen must not evict: the tenant is still there, so the
// caches would simply re-read it, and the log would claim work that was not done.
func TestAFailedPurgeEvictsNothing(t *testing.T) {
	db, _ := purgeFixture(t)
	spy := &spyCache{}
	ev := NewEvictor(nil, nil)
	ev.Register(spy)

	j := NewPurgeJob(db, nil, 0)
	j.SetEvictor(ev)
	// The TAK teardown fails, which stops this tenant's purge before PurgeTenant.
	j.SetTAKInstances(&failingTAKDeleter{})
	j.once(context.Background())

	if got := spy.seen(); len(got) != 0 {
		t.Errorf("evicted %v after a purge that was abandoned", got)
	}
}

type failingTAKDeleter struct{}

func (failingTAKDeleter) DeleteTAKInstanceForTenant(context.Context, string) error {
	return errors.New("cluster unreachable")
}
