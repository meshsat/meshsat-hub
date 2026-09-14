package integrations

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// twoReplicas is the only way to see MESHSAT-1127 in a test: the bug is that
// one process does not know what another process wrote, so one Service cannot
// show it. These two share a store, the way two Hub pods share Postgres, and a
// loopback bus stands in for the broker.
type loopbackBus struct {
	mu   sync.Mutex
	subs map[string][]bus.MessageHandler
}

func newLoopback() *loopbackBus { return &loopbackBus{subs: map[string][]bus.MessageHandler{}} }

func (b *loopbackBus) Connect() error { return nil }
func (b *loopbackBus) Publish(topic string, _ byte, _ bool, payload []byte) error {
	b.mu.Lock()
	hs := append([]bus.MessageHandler(nil), b.subs[topic]...)
	b.mu.Unlock()
	for _, h := range hs {
		h(topic, payload)
	}
	return nil
}
func (b *loopbackBus) PublishJSON(topic string, q byte, r bool, v any) error {
	p, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return b.Publish(topic, q, r, p)
}
func (b *loopbackBus) Subscribe(topic string, _ byte, h bus.MessageHandler) error {
	b.mu.Lock()
	b.subs[topic] = append(b.subs[topic], h)
	b.mu.Unlock()
	return nil
}
func (b *loopbackBus) QueueSubscribe(t string, q byte, _ string, h bus.MessageHandler) error {
	return b.Subscribe(t, q, h)
}
func (b *loopbackBus) IsConnected() bool { return true }
func (b *loopbackBus) Disconnect()       {}

var _ bus.MessageBus = (*loopbackBus)(nil)

func twoReplicas(t *testing.T) (a, b *Service, ctx context.Context) {
	t.Helper()
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx = context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	bs := newLoopback()
	a, b = New(db, key), New(db, key)
	a.noURLCheck, b.noURLCheck = true, true
	if err := a.SetBus(bs); err != nil {
		t.Fatalf("a.SetBus: %v", err)
	}
	if err := b.SetBus(bs); err != nil {
		t.Fatalf("b.SetBus: %v", err)
	}
	return a, b, ctx
}

// THE ONE THAT MATTERS. A cached NEGATIVE is the damaging direction: the replica
// believes the tenant has no credentials, so it does not send the SMS, does not
// deliver the notification and does not inject the position. It is also the one
// a naive fix misses, because "invalidate on write" reads as being about
// updating a value that is already there.
func TestACachedNotConfiguredIsInvalidatedOnTheOtherReplica(t *testing.T) {
	a, b, ctx := twoReplicas(t)

	// Replica B reads first and caches "this tenant has no ntfy account".
	got, err := b.ForTenant(ctx, "t1", ProviderNtfy)
	if err != nil {
		t.Fatalf("b.ForTenant: %v", err)
	}
	if got != nil {
		t.Fatal("fixture is wrong: the tenant should start with no account")
	}

	// The customer saves it, and the write lands on replica A.
	if _, err := a.Set(ctx, "t1", ProviderNtfy, map[string]string{"url": "https://ntfy.example.com"}); err != nil {
		t.Fatalf("a.Set: %v", err)
	}

	// B must now see it, without waiting out its TTL.
	got, err = b.ForTenant(ctx, "t1", ProviderNtfy)
	if err != nil {
		t.Fatalf("b.ForTenant after set: %v", err)
	}
	if got == nil {
		t.Fatal("the replica that did not serve the write still believes the tenant has NO account. " +
			"For up to the cache TTL it will not send that tenant's SMS, deliver its notifications " +
			"or inject its positions, and roughly half the traffic is handled by it.")
	}
	if got.Get("url") != "https://ntfy.example.com" {
		t.Errorf("wrong account propagated: %q", got.Get("url"))
	}
}

// A changed account must propagate too, not only a first save. Otherwise a
// customer who corrects a wrong URL keeps being served by the old one.
func TestAChangedAccountReachesTheOtherReplica(t *testing.T) {
	a, b, ctx := twoReplicas(t)
	if _, err := a.Set(ctx, "t1", ProviderNtfy, map[string]string{"url": "https://old.example.com"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got, _ := b.ForTenant(ctx, "t1", ProviderNtfy); got == nil || got.Get("url") != "https://old.example.com" {
		t.Fatalf("b did not see the first save")
	}
	if _, err := a.Set(ctx, "t1", ProviderNtfy, map[string]string{"url": "https://new.example.com"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := b.ForTenant(ctx, "t1", ProviderNtfy)
	if got == nil || got.Get("url") != "https://new.example.com" {
		t.Fatalf("the other replica is still serving the OLD url: %v", got)
	}
}

// Deleting must propagate as well: a revoked credential that keeps working on
// one replica of two is the security half of the same bug.
func TestADeletedAccountStopsResolvingOnTheOtherReplica(t *testing.T) {
	a, b, ctx := twoReplicas(t)
	if _, err := a.Set(ctx, "t1", ProviderNtfy, map[string]string{"url": "https://ntfy.example.com"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got, _ := b.ForTenant(ctx, "t1", ProviderNtfy); got == nil {
		t.Fatal("b did not see the save")
	}
	if err := a.Delete(ctx, "t1", ProviderNtfy); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got, _ := b.ForTenant(ctx, "t1", ProviderNtfy); got != nil {
		t.Fatal("the revoked account still resolves on the other replica")
	}
}

// One tenant's change must not flush another's, and must never make another
// tenant's account resolve.
func TestOneTenantsChangeDoesNotDisturbAnother(t *testing.T) {
	a, b, ctx := twoReplicas(t)
	if _, err := a.Set(ctx, "t1", ProviderNtfy, map[string]string{"url": "https://one.example.com"}); err != nil {
		t.Fatalf("set t1: %v", err)
	}
	if _, err := a.Set(ctx, "t2", ProviderNtfy, map[string]string{"url": "https://two.example.com"}); err != nil {
		t.Fatalf("set t2: %v", err)
	}
	if got, _ := b.ForTenant(ctx, "t1", ProviderNtfy); got == nil || got.Get("url") != "https://one.example.com" {
		t.Errorf("t1 resolved to %v", got)
	}
	if got, _ := b.ForTenant(ctx, "t2", ProviderNtfy); got == nil || got.Get("url") != "https://two.example.com" {
		t.Errorf("t2 resolved to %v", got)
	}
	// And a tenant that has nothing still has nothing.
	if got, _ := b.ForTenant(ctx, "t3", ProviderNtfy); got != nil {
		t.Errorf("LEAK: t3 has no account of its own but resolved %v", got)
	}
}

// Without a bus the service must still work. A self-hosted single-replica Hub
// has no broker subscription to make, and the TTL is the backstop anyway.
func TestNoBusIsNotAFailure(t *testing.T) {
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := New(db, make([]byte, 32))
	s.noURLCheck = true
	if err := s.SetBus(nil); err != nil {
		t.Fatalf("SetBus(nil) should be a no-op, got %v", err)
	}
	if _, err := s.Set(ctx, store.DefaultTenantID, ProviderNtfy, map[string]string{"url": "https://x.example.com"}); err != nil {
		t.Fatalf("Set with no bus: %v", err)
	}
	if got, _ := s.ForTenant(ctx, store.DefaultTenantID, ProviderNtfy); got == nil {
		t.Fatal("the account did not save with no bus attached")
	}
}

// flakyBus refuses the first n Subscribe calls, the way the real bus does while
// the broker is not up yet: it calls the broker, waits, and returns an error
// WITHOUT registering the handler, so there is nothing for the reconnect
// handler to replay later.
type flakyBus struct {
	*loopbackBus
	remainingFailures int
}

func (f *flakyBus) Subscribe(topic string, q byte, h bus.MessageHandler) error {
	if f.remainingFailures > 0 {
		f.remainingFailures--
		return errNotConnected
	}
	return f.loopbackBus.Subscribe(topic, q, h)
}

var errNotConnected = &notConnected{}

type notConnected struct{}

func (*notConnected) Error() string { return "bus: not currently connected" }

// SetBus must REPORT a failed subscription rather than swallow it, and must
// work when called again.
//
// This is the contract the caller's retry loop depends on. Without it, a broker
// that is down for the few seconds around startup leaves that replica with no
// cross-replica sync for the life of the process -- silently, with only the
// cache TTL underneath. The first version of this fix gated a single attempt on
// IsConnected() and did exactly that.
func TestSetBusReportsAFailedSubscriptionAndCanBeRetried(t *testing.T) {
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	key := make([]byte, 32)
	shared := newLoopback()
	fb := &flakyBus{loopbackBus: shared, remainingFailures: 2}

	writer, reader := New(db, key), New(db, key)
	writer.noURLCheck, reader.noURLCheck = true, true
	if err := writer.SetBus(shared); err != nil {
		t.Fatalf("writer.SetBus: %v", err)
	}

	// The reader's broker is down: two attempts fail, and they must SAY so.
	if err := reader.SetBus(fb); err == nil {
		t.Fatal("SetBus swallowed a failed subscription; the caller cannot know to retry " +
			"and this replica would never receive another account change")
	}
	if err := reader.SetBus(fb); err == nil {
		t.Fatal("second attempt also had to fail for this fixture to mean anything")
	}

	// Third attempt succeeds, as the retry loop in main.go would find.
	if err := reader.SetBus(fb); err != nil {
		t.Fatalf("third attempt should have succeeded: %v", err)
	}

	// And the subscription is real: a write on the other service reaches it.
	if _, err := reader.ForTenant(ctx, "t1", ProviderNtfy); err != nil {
		t.Fatalf("seed read: %v", err)
	}
	if _, err := writer.Set(ctx, "t1", ProviderNtfy, map[string]string{"url": "https://ntfy.example.com"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got, _ := reader.ForTenant(ctx, "t1", ProviderNtfy); got == nil {
		t.Fatal("the retried subscription is not actually receiving announcements")
	}
}
