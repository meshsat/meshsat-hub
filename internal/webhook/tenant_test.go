package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1118. WebhookConfig carried no tenant at all and Fire selected its
// targets by event type alone, so every registered webhook received EVERY
// tenant's message contents, positions, telemetry and SOS events. A customer
// registering one webhook was subscribing to the whole platform's traffic.
//
// The other half was just as wrong in the opposite direction: Start subscribed
// to "meshsat/+/mo/decoded", which is four segments and matches only the
// DEFAULT tenant's topics. So a customer's webhook received the platform
// tenant's traffic and never its own.
//
// Nothing was leaking in production when this was found -- zero webhooks were
// configured -- which is the only reason this reads as a defect rather than an
// incident.

const (
	tenantA = "t_alpha"
	tenantB = "t_beta"
)

// recorder is a webhook endpoint that counts what it was sent.
type recorder struct {
	mu   sync.Mutex
	hits []WebhookPayload
	srv  *httptest.Server
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	r := &recorder{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var p WebhookPayload
		_ = json.Unmarshal(body, &p)
		r.mu.Lock()
		r.hits = append(r.hits, p)
		r.mu.Unlock()
		w.WriteHeader(200)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.hits)
}

// The headline test. The victim is store.DefaultTenantID deliberately: the
// original code had no tenant dimension at all, so a regression that drops the
// filter lands everything on whichever webhooks exist. Using two non-default
// tenants would let a regression to "the default tenant" pass by accident --
// the same trap mutation testing caught in internal/deadman/tenant_test.go.
func TestOneTenantsEventNeverReachesAnothersWebhook(t *testing.T) {
	victim := newRecorder(t)
	attacker := newRecorder(t)

	d := NewDispatcher(nil)
	d.AllowLoopbackTargetsForTest()
	d.AddWebhook(WebhookConfig{
		ID: "victim", TenantID: store.DefaultTenantID, URL: victim.srv.URL,
		Events: []EventType{EventSOS}, Enabled: true,
	})
	d.AddWebhook(WebhookConfig{
		ID: "attacker", TenantID: tenantB, URL: attacker.srv.URL,
		Events: []EventType{EventSOS}, Enabled: true,
	})

	// The default tenant raises an SOS.
	d.Fire(store.DefaultTenantID, EventSOS, "dev-victim", "probe-"+"dev-victim", json.RawMessage(`{"triggered":true}`))
	time.Sleep(300 * time.Millisecond)

	if victim.count() != 1 {
		t.Errorf("the tenant's own webhook got %d deliveries, want 1", victim.count())
	}
	if attacker.count() != 0 {
		t.Errorf("another tenant's webhook received %d of the default tenant's SOS events. "+
			"This is the distress location of somebody else's device leaving the platform.",
			attacker.count())
	}
}

// An event with no tenant must reach nobody. There is no safe default here:
// "unknown tenant" cannot mean "all tenants", and it must not mean "whichever
// webhook also has no tenant" either.
//
// The second case is the one that needs the explicit guard in Fire, and it is
// reachable: webhook_configs.tenant_id has a default but no NOT NULL check that
// a hand-written row must satisfy, and a config built in code can leave the
// field zero. Without the guard, an empty tenant on both sides compares EQUAL
// and such a row becomes a catch-all for every event the Hub cannot attribute.
func TestAnEventWithNoTenantFiresNothing(t *testing.T) {
	owned := newRecorder(t)
	orphan := newRecorder(t)
	d := NewDispatcher(nil)
	d.AllowLoopbackTargetsForTest()
	d.AddWebhook(WebhookConfig{
		ID: "owned", TenantID: store.DefaultTenantID, URL: owned.srv.URL,
		Events: []EventType{EventMO}, Enabled: true,
	})
	d.AddWebhook(WebhookConfig{
		ID: "orphan", TenantID: "", URL: orphan.srv.URL,
		Events: []EventType{EventMO}, Enabled: true,
	})

	d.Fire("", EventMO, "dev-1", "probe-"+"dev-1", json.RawMessage(`{}`))
	time.Sleep(200 * time.Millisecond)

	if owned.count() != 0 {
		t.Errorf("a tenant-less event was delivered to a tenant's webhook %d times", owned.count())
	}
	if orphan.count() != 0 {
		t.Errorf("a tenant-less event was delivered %d times to a webhook that itself has no "+
			"tenant. Empty == empty is not a match; it is two unknowns.", orphan.count())
	}
}

func TestListingShowsOnlyTheTenantsOwnWebhooks(t *testing.T) {
	d := NewDispatcher(nil)
	d.AddWebhook(WebhookConfig{ID: "a1", TenantID: tenantA, URL: "https://a.example", Enabled: true})
	d.AddWebhook(WebhookConfig{ID: "b1", TenantID: store.DefaultTenantID, URL: "https://b.example", Enabled: true})

	got := d.ListWebhooks(tenantA)
	if len(got) != 1 || got[0].ID != "a1" {
		t.Fatalf("tenant A sees %v, want only a1 -- the list used to return every tenant's "+
			"webhooks, target URLs included", got)
	}
	if len(d.ListWebhooks(store.DefaultTenantID)) != 1 {
		t.Errorf("the default tenant should still see its own one webhook")
	}
}

func TestATenantCannotDeleteAnothersWebhook(t *testing.T) {
	d := NewDispatcher(nil)
	// Victim is the default tenant, for the reason on the isolation test above.
	d.AddWebhook(WebhookConfig{ID: "shared-id", TenantID: store.DefaultTenantID, URL: "https://v.example", Enabled: true})

	if d.RemoveWebhook(tenantB, "shared-id") {
		t.Error("RemoveWebhook reported removing a webhook belonging to another tenant")
	}
	if len(d.ListWebhooks(store.DefaultTenantID)) != 1 {
		t.Error("another tenant deleted the default tenant's webhook")
	}
	if !d.RemoveWebhook(store.DefaultTenantID, "shared-id") {
		t.Error("the owning tenant could not remove its own webhook")
	}
}

func TestDeliveryLogsAreScopedToTheTenant(t *testing.T) {
	d := NewDispatcher(nil)
	d.recordLog(DeliveryLog{TenantID: store.DefaultTenantID, WebhookID: "wh-v", Event: "sos", DeviceID: "dev-victim", StatusCode: 200, Error: "", Attempt: 0})
	d.recordLog(DeliveryLog{TenantID: tenantB, WebhookID: "wh-a", Event: "mo", DeviceID: "dev-other", StatusCode: 500, Error: "boom", Attempt: 1})

	got := d.RecentLogs(tenantB, 100)
	if len(got) != 1 || got[0].WebhookID != "wh-a" {
		t.Fatalf("tenant B sees %v, want only its own entry", got)
	}
	// The log carries the device id, which is the part that would leak.
	for _, l := range got {
		if l.DeviceID == "dev-victim" {
			t.Error("another tenant's device id appeared in this tenant's delivery log")
		}
	}
}

// A busy neighbour must not push a tenant's own entries out of its window.
// A "take the last N rows, then filter" implementation passes every test above
// and fails this one.
func TestABusyTenantDoesNotCrowdOutAnothersLogs(t *testing.T) {
	d := NewDispatcher(nil)
	d.recordLog(DeliveryLog{TenantID: tenantA, WebhookID: "wh-a", Event: "mo", DeviceID: "dev-a", StatusCode: 200, Error: "", Attempt: 0})
	for i := 0; i < 200; i++ {
		d.recordLog(DeliveryLog{TenantID: tenantB, WebhookID: "wh-b", Event: "mo", DeviceID: "dev-b", StatusCode: 200, Error: "", Attempt: 0})
	}
	if got := d.RecentLogs(tenantA, 100); len(got) != 1 {
		t.Errorf("tenant A sees %d of its own log entries, want 1", len(got))
	}
}

func TestForgetTenantDropsTheWebhooksAndTheLogs(t *testing.T) {
	d := NewDispatcher(nil)
	d.AddWebhook(WebhookConfig{ID: "gone", TenantID: tenantA, URL: "https://a.example", Enabled: true})
	d.AddWebhook(WebhookConfig{ID: "stays", TenantID: store.DefaultTenantID, URL: "https://b.example", Enabled: true})
	d.recordLog(DeliveryLog{TenantID: tenantA, WebhookID: "gone", Event: "mo", DeviceID: "dev-a", StatusCode: 200, Error: "", Attempt: 0})

	d.ForgetTenant(tenantA)

	if len(d.ListWebhooks(tenantA)) != 0 {
		t.Error("a purged tenant's webhook is still a live outbound target")
	}
	if len(d.RecentLogs(tenantA, 100)) != 0 {
		t.Error("a purged tenant's delivery logs are still held in memory")
	}
	if len(d.ListWebhooks(store.DefaultTenantID)) != 1 {
		t.Error("ForgetTenant removed another tenant's webhook")
	}
}

// The dispatcher must satisfy tenancy.TenantForgetter. The interface is
// declared in internal/tenancy and asserting it there would make that package
// import this one, so the check lives here as a compile-time assertion against
// a local copy of the one method.
var _ interface{ ForgetTenant(string) } = (*Dispatcher)(nil)

// Both Hub replicas subscribe to every MQTT topic with a plain Subscribe, not a
// queue group, so BOTH receive every message and both reach Fire. Without a
// claim the customer's endpoint is POSTed to twice for one event.
//
// This got worse rather than better when webhooks were persisted and synced
// across replicas: before that only one replica held any webhook, so the
// duplicate was prevented by accident. Once both hold every webhook it is
// certain.
//
// Two dispatchers sharing one fakeStore ARE two replicas sharing one database.
func TestTwoReplicasDeliverAWebhookOnce(t *testing.T) {
	rec := newRecorder(t)
	fs := newFakeStore(store.DefaultTenantID)

	cfg := WebhookConfig{
		ID: "wh-1", TenantID: store.DefaultTenantID, URL: rec.srv.URL,
		Events: []EventType{EventSOS}, Enabled: true,
	}
	replicas := make([]*Dispatcher, 2)
	for i := range replicas {
		d := NewDispatcher(nil)
		d.AllowLoopbackTargetsForTest()
		d.SetStore(fs)
		d.AddWebhook(cfg)
		replicas[i] = d
	}

	// The same message reaches both, so both compute the same dedup key.
	const msg = "mo-deadbeefdeadbeef"
	for _, d := range replicas {
		d.Fire(store.DefaultTenantID, EventSOS, "dev-1", msg, json.RawMessage(`{"triggered":true}`))
	}
	time.Sleep(400 * time.Millisecond)

	if got := rec.count(); got != 1 {
		t.Errorf("the customer's endpoint was POSTed to %d times for one event, want 1. "+
			"Both replicas receive every message; only one may deliver.", got)
	}
}

// Two DIFFERENT webhooks on the same event are two deliveries, and neither may
// be swallowed by the other's claim.
func TestTwoWebhooksOnOneEventBothDeliver(t *testing.T) {
	a, b := newRecorder(t), newRecorder(t)
	fs := newFakeStore(store.DefaultTenantID)
	d := NewDispatcher(nil)
	d.AllowLoopbackTargetsForTest()
	d.SetStore(fs)
	d.AddWebhook(WebhookConfig{ID: "wh-a", TenantID: store.DefaultTenantID, URL: a.srv.URL,
		Events: []EventType{EventSOS}, Enabled: true})
	d.AddWebhook(WebhookConfig{ID: "wh-b", TenantID: store.DefaultTenantID, URL: b.srv.URL,
		Events: []EventType{EventSOS}, Enabled: true})

	d.Fire(store.DefaultTenantID, EventSOS, "dev-1", "mo-1", json.RawMessage(`{}`))
	time.Sleep(400 * time.Millisecond)

	if a.count() != 1 || b.count() != 1 {
		t.Errorf("deliveries a=%d b=%d, want 1 each: the claim is per webhook, not per "+
			"message, or one endpoint silently stops receiving", a.count(), b.count())
	}
}

// A LATER message to the same webhook must still be delivered -- the claim
// identifies the message, not the webhook.
func TestASecondMessageIsStillDelivered(t *testing.T) {
	rec := newRecorder(t)
	fs := newFakeStore(store.DefaultTenantID)
	d := NewDispatcher(nil)
	d.AllowLoopbackTargetsForTest()
	d.SetStore(fs)
	d.AddWebhook(WebhookConfig{ID: "wh-1", TenantID: store.DefaultTenantID, URL: rec.srv.URL,
		Events: []EventType{EventMO}, Enabled: true})

	d.Fire(store.DefaultTenantID, EventMO, "dev-1", "mo-first", json.RawMessage(`{}`))
	d.Fire(store.DefaultTenantID, EventMO, "dev-1", "mo-second", json.RawMessage(`{}`))
	time.Sleep(400 * time.Millisecond)

	if got := rec.count(); got != 2 {
		t.Errorf("got %d deliveries for two distinct messages, want 2", got)
	}
}

// The claim FAILS OPEN. A webhook that arrives twice is a nuisance; one that
// never arrives because the database hiccuped is a lost event, and the payload
// carries its own id for a receiver that cares.
func TestAFailedClaimStillDelivers(t *testing.T) {
	rec := newRecorder(t)
	fs := newFakeStore(store.DefaultTenantID)
	fs.claimErr = context.DeadlineExceeded
	d := NewDispatcher(nil)
	d.AllowLoopbackTargetsForTest()
	d.SetStore(fs)
	d.AddWebhook(WebhookConfig{ID: "wh-1", TenantID: store.DefaultTenantID, URL: rec.srv.URL,
		Events: []EventType{EventMO}, Enabled: true})

	d.Fire(store.DefaultTenantID, EventMO, "dev-1", "mo-1", json.RawMessage(`{}`))
	time.Sleep(400 * time.Millisecond)

	if got := rec.count(); got != 1 {
		t.Errorf("got %d deliveries when the claim errored, want 1: this must fail open", got)
	}
}
