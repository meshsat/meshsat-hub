package webhook

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// fakeBus records what was subscribed and lets a test deliver a message.
type fakeBus struct {
	mu        sync.Mutex
	handlers  map[string][]bus.MessageHandler
	published []string
}

func newFakeBus() *fakeBus { return &fakeBus{handlers: map[string][]bus.MessageHandler{}} }

func (b *fakeBus) Connect() error { return nil }
func (b *fakeBus) Publish(topic string, _ byte, _ bool, _ []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published = append(b.published, topic)
	return nil
}
func (b *fakeBus) PublishJSON(topic string, q byte, r bool, _ any) error {
	return b.Publish(topic, q, r, nil)
}
func (b *fakeBus) Subscribe(topic string, _ byte, h bus.MessageHandler) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[topic] = append(b.handlers[topic], h)
	return nil
}
func (b *fakeBus) QueueSubscribe(topic string, q byte, _ string, h bus.MessageHandler) error {
	return b.Subscribe(topic, q, h)
}
func (b *fakeBus) IsConnected() bool { return true }
func (b *fakeBus) Disconnect()       {}

func (b *fakeBus) deliver(topic string, payload []byte) {
	b.mu.Lock()
	var hs []bus.MessageHandler
	for filter, list := range b.handlers {
		if matches(filter, topic) {
			hs = append(hs, list...)
		}
	}
	b.mu.Unlock()
	for _, h := range hs {
		h(topic, payload)
	}
}

// matches is MQTT filter matching, enough for the shapes used here.
func matches(filter, topic string) bool {
	f, tp := split(filter), split(topic)
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

func split(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '/' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}

// The subscription shape is the half of MESHSAT-1118 that looked like nothing.
// Start subscribed to "meshsat/+/mo/decoded" and the four other legacy filters.
// Those have four segments and match ONLY the default tenant's topics, so a
// customer's webhook could never receive a single one of its own tenant's
// events -- while the platform tenant's events reached every webhook on the
// platform. Both halves are in this one test.
func TestEventsReachOnlyTheOwningTenantsWebhook(t *testing.T) {
	b := newFakeBus()
	d := NewDispatcher(b)
	d.AllowLoopbackTargetsForTest()

	customer := newRecorder(t)
	platform := newRecorder(t)
	d.AddWebhook(WebhookConfig{
		ID: "customer", TenantID: tenantA, URL: customer.srv.URL,
		Events: []EventType{EventMO}, Enabled: true,
	})
	d.AddWebhook(WebhookConfig{
		ID: "platform", TenantID: store.DefaultTenantID, URL: platform.srv.URL,
		Events: []EventType{EventMO}, Enabled: true,
	})

	if err := d.Start(b); err != nil {
		t.Fatalf("start: %v", err)
	}

	// A message on the CUSTOMER's five-segment topic.
	b.deliver("meshsat/"+tenantA+"/dev-a/mo/decoded", []byte(`{"text":"hello"}`))
	time.Sleep(300 * time.Millisecond)

	if customer.count() != 1 {
		t.Errorf("the customer's webhook got %d deliveries of its own tenant's message, want 1. "+
			"The legacy-only filter never matched a non-default tenant's topic.", customer.count())
	}
	if platform.count() != 0 {
		t.Errorf("the platform tenant's webhook received %d of a customer's messages",
			platform.count())
	}
}

// And the legacy four-segment shape still works, because the default tenant
// publishes on it.
func TestTheLegacyTopicShapeStillReachesTheDefaultTenant(t *testing.T) {
	b := newFakeBus()
	d := NewDispatcher(b)
	d.AllowLoopbackTargetsForTest()

	rec := newRecorder(t)
	d.AddWebhook(WebhookConfig{
		ID: "platform", TenantID: store.DefaultTenantID, URL: rec.srv.URL,
		Events: []EventType{EventSOS}, Enabled: true,
	})
	if err := d.Start(b); err != nil {
		t.Fatalf("start: %v", err)
	}

	b.deliver("meshsat/dev-p/sos", []byte(`{"triggered":true}`))
	time.Sleep(300 * time.Millisecond)

	if rec.count() != 1 {
		t.Errorf("the default tenant's webhook got %d deliveries, want 1", rec.count())
	}
}

// A change made here is announced so the other replica reloads it. Without
// this a webhook created on one of the two replicas fires for that replica's
// share of the traffic only, which reads as a flaky customer endpoint.
func TestAChangeIsAnnouncedToTheOtherReplicas(t *testing.T) {
	b := newFakeBus()
	fs := newFakeStore(tenantA)
	d := NewDispatcher(b)
	d.AllowLoopbackTargetsForTest()
	d.SetStore(fs)

	if err := d.Save(t.Context(), WebhookConfig{
		ID: "a1", TenantID: tenantA, URL: "https://a.example", Enabled: true,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	b.mu.Lock()
	got := append([]string(nil), b.published...)
	b.mu.Unlock()
	if len(got) != 1 || got[0] != ReloadTopic {
		t.Fatalf("published %v, want one announcement on %s", got, ReloadTopic)
	}
}

// The other replica's side of that: an announcement makes it reload exactly
// the named tenant, and leaves every other tenant's webhooks alone.
func TestAnAnnouncementReloadsOnlyTheNamedTenant(t *testing.T) {
	b := newFakeBus()
	fs := newFakeStore(tenantA, store.DefaultTenantID)
	// Tenant A has one webhook in the database that this replica has not seen.
	_ = fs.SaveWebhook(t.Context(), tenantA, &store.WebhookConfig{
		ID: "a1", URL: "https://a.example", Events: []string{"mo"}, Enabled: true,
	})

	replica := NewDispatcher(b)
	replica.SetStore(fs)
	replica.AddWebhook(WebhookConfig{
		ID: "untouched", TenantID: store.DefaultTenantID, URL: "https://p.example", Enabled: true,
	})
	if err := replica.Start(b); err != nil {
		t.Fatalf("start: %v", err)
	}

	payload, _ := json.Marshal(reloadEvent{TenantID: tenantA})
	b.deliver(ReloadTopic, payload)

	if got := replica.ListWebhooks(tenantA); len(got) != 1 || got[0].ID != "a1" {
		t.Errorf("after the announcement tenant A has %v, want its one webhook", got)
	}
	if got := replica.ListWebhooks(store.DefaultTenantID); len(got) != 1 {
		t.Errorf("reloading tenant A disturbed the default tenant: %v", got)
	}
}
