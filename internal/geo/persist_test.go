package geo

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1119. Fences lived in one replica's map and nothing wrote them down,
// so a customer's fence was gone at the next rollout -- and because the engine
// never evaluated anything either, nobody had noticed.

type fenceStore struct {
	mu      sync.Mutex
	rows    map[string][]store.Geofence
	tenants []string
	saveErr error
}

func newFenceStore(tenants ...string) *fenceStore {
	return &fenceStore{rows: map[string][]store.Geofence{}, tenants: tenants}
}

func (f *fenceStore) ListTenants(context.Context) ([]store.Tenant, error) {
	out := make([]store.Tenant, 0, len(f.tenants))
	for _, id := range f.tenants {
		out = append(out, store.Tenant{ID: id})
	}
	return out, nil
}

func (f *fenceStore) ListGeofences(_ context.Context, tenantID string) ([]store.Geofence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Geofence(nil), f.rows[tenantID]...), nil
}

func (f *fenceStore) SaveGeofence(_ context.Context, tenantID string, g *store.Geofence) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	for i, existing := range f.rows[tenantID] {
		if existing.ID == g.ID {
			f.rows[tenantID][i] = *g
			return nil
		}
	}
	f.rows[tenantID] = append(f.rows[tenantID], *g)
	return nil
}

func (f *fenceStore) DeleteGeofence(_ context.Context, tenantID, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.rows[tenantID][:0]
	for _, g := range f.rows[tenantID] {
		if g.ID != id {
			kept = append(kept, g)
		}
	}
	f.rows[tenantID] = kept
	return nil
}

func TestAFenceSurvivesARestart(t *testing.T) {
	fs := newFenceStore(testTenant, otherTenant)
	e := NewEngine()
	e.SetStore(fs)

	f := fenceIn(testTenant, "perimeter")
	if err := e.Save(context.Background(), f); err != nil {
		t.Fatalf("save: %v", err)
	}

	// A fresh engine, as a restarted replica has.
	fresh := NewEngine()
	fresh.SetStore(fs)
	if err := fresh.LoadAll(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}

	got := fresh.ListFences(testTenant)
	if len(got) != 1 {
		t.Fatalf("after a restart the tenant has %d fences, want 1", len(got))
	}
	if got[0].ChainID != f.ChainID {
		t.Errorf("the escalation chain did not survive: %q, want %q", got[0].ChainID, f.ChainID)
	}
	if len(got[0].Polygon) != len(f.Polygon) {
		t.Fatalf("the polygon has %d vertices after a reload, want %d. A fence whose "+
			"geometry is lost matches nothing and pages nobody, silently.",
			len(got[0].Polygon), len(f.Polygon))
	}
	for i := range f.Polygon {
		if got[0].Polygon[i] != f.Polygon[i] {
			t.Errorf("vertex %d = %+v, want %+v", i, got[0].Polygon[i], f.Polygon[i])
		}
	}
	if got[0].Trigger != f.Trigger || !got[0].Enabled || got[0].TenantID != testTenant {
		t.Errorf("reloaded fence = %+v", got[0])
	}

	// And it actually fires after the reload, which is the point of persisting.
	c := &collector{}
	fresh.OnEvent(c.handle)
	fresh.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5, time.Time{})
	if n := len(c.all()); n != 1 {
		t.Errorf("a reloaded fence produced %d events on a crossing, want 1", n)
	}
}

// A failed write must not report success: the customer would see a fence in the
// list that disappears at the next rollout.
func TestSaveFailsLoudlyWhenTheDatabaseRefuses(t *testing.T) {
	fs := newFenceStore(testTenant)
	fs.saveErr = context.DeadlineExceeded
	e := NewEngine()
	e.SetStore(fs)

	if err := e.Save(context.Background(), fenceIn(testTenant, "perimeter")); err == nil {
		t.Fatal("Save reported success on a failed write")
	}
	if got := e.ListFences(testTenant); len(got) != 0 {
		t.Errorf("the fence was armed in memory despite the failed write: %v", got)
	}
}

// A delete removes the row as well as the in-memory fence, or the fence comes
// back at the next restart.
func TestDeleteRemovesTheRowToo(t *testing.T) {
	fs := newFenceStore(testTenant)
	e := NewEngine()
	e.SetStore(fs)
	if err := e.Save(context.Background(), fenceIn(testTenant, "perimeter")); err != nil {
		t.Fatalf("save: %v", err)
	}

	if _, err := e.Delete(context.Background(), testTenant, "perimeter"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rows, _ := fs.ListGeofences(context.Background(), testTenant)
	if len(rows) != 0 {
		t.Errorf("the row survived the delete: %v -- the fence returns at the next restart", rows)
	}
}

// An engine with no store still works, which is what a store-less deployment
// and most of the tests in this package have.
func TestAnEngineWithNoStoreStillWorks(t *testing.T) {
	e := NewEngine()
	if err := e.LoadAll(context.Background()); err != nil {
		t.Fatalf("LoadAll with no store: %v", err)
	}
	if err := e.Save(context.Background(), fenceIn(testTenant, "f")); err != nil {
		t.Fatalf("Save with no store: %v", err)
	}
	if len(e.ListFences(testTenant)) != 1 {
		t.Error("the fence was not armed in memory")
	}
}

// Both Hub replicas evaluate the same position. A fence created through the API
// lands on ONE of them, so without this every crossing depended on which replica
// the create happened to reach -- and the live probe that found it passed the
// first time and failed the second for exactly that reason.
func TestAReloadArmsAFenceCreatedElsewhere(t *testing.T) {
	fs := newFenceStore(testTenant)
	// "The other replica": same database, its own memory, knows nothing yet.
	other := NewEngine()
	other.SetStore(fs)

	// A fence is created on the first replica.
	first := NewEngine()
	first.SetStore(fs)
	if err := first.Save(context.Background(), fenceIn(testTenant, "perimeter")); err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(other.ListFences(testTenant)) != 0 {
		t.Fatal("precondition: the other replica should not have it yet")
	}

	// The announcement reaches it.
	if err := other.ReloadTenant(context.Background(), testTenant); err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := other.ListFences(testTenant)
	if len(got) != 1 || got[0].ID != "perimeter" {
		t.Fatalf("after the reload the other replica has %v, want the fence", got)
	}

	c := &collector{}
	other.OnEvent(c.handle)
	other.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5, time.Time{})
	if len(c.all()) != 1 {
		t.Error("the reloaded fence did not fire on a crossing")
	}
}

// A reload must not clear crossing state: a device already inside a fence is
// still inside it, and forgetting that pages somebody for a crossing that never
// happened.
func TestAReloadDoesNotInventACrossing(t *testing.T) {
	fs := newFenceStore(testTenant)
	e := NewEngine()
	e.SetStore(fs)
	if err := e.Save(context.Background(), fenceIn(testTenant, "perimeter")); err != nil {
		t.Fatalf("save: %v", err)
	}
	c := &collector{}
	e.OnEvent(c.handle)

	e.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5, time.Time{}) // enter, fires
	if n := len(c.all()); n != 1 {
		t.Fatalf("got %d events, want 1", n)
	}

	if err := e.ReloadTenant(context.Background(), testTenant); err != nil {
		t.Fatalf("reload: %v", err)
	}
	e.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5, time.Time{}) // still inside
	if n := len(c.all()); n != 1 {
		t.Errorf("got %d events after a reload, want 1: the reload forgot the device was "+
			"already inside and reported a crossing that never happened", n)
	}
}

// The event carries the POSITION's time, not the processing time, because both
// replicas key their once-only claim on it.
func TestTheEventCarriesThePositionsOwnTime(t *testing.T) {
	e := NewEngine()
	c := &collector{}
	e.OnEvent(c.handle)
	e.AddFence(fenceIn(testTenant, "perimeter"))

	at := time.Date(2026, 9, 14, 1, 2, 3, 0, time.UTC)
	e.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5, at)

	got := c.all()
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if !got[0].Timestamp.Equal(at) {
		t.Errorf("event time %s, want the position's %s. Both replicas claim the crossing "+
			"using this value; a processing clock differs between them and pages twice.",
			got[0].Timestamp, at)
	}
}

// announceBus records what the engine told the other replicas.
type announceBus struct {
	mu        sync.Mutex
	published []string
	handler   bus.MessageHandler
}

func (b *announceBus) Connect() error { return nil }
func (b *announceBus) Publish(topic string, _ byte, _ bool, payload []byte) error {
	b.mu.Lock()
	b.published = append(b.published, topic)
	h := b.handler
	b.mu.Unlock()
	// Deliver to the subscriber, the way the broker would.
	if h != nil {
		h(topic, payload)
	}
	return nil
}
func (b *announceBus) PublishJSON(t string, q byte, r bool, _ any) error {
	return b.Publish(t, q, r, nil)
}
func (b *announceBus) Subscribe(_ string, _ byte, h bus.MessageHandler) error {
	b.mu.Lock()
	b.handler = h
	b.mu.Unlock()
	return nil
}
func (b *announceBus) QueueSubscribe(t string, q byte, _ string, h bus.MessageHandler) error {
	return b.Subscribe(t, q, h)
}
func (b *announceBus) IsConnected() bool { return true }
func (b *announceBus) Disconnect()       {}

func (b *announceBus) topics() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.published...)
}

// Creating or deleting a fence has to be announced, or the other replica stays
// unaware until it restarts and whether a crossing is noticed depends on which
// replica served the create.
func TestFenceChangesAreAnnouncedToOtherReplicas(t *testing.T) {
	fs := newFenceStore(testTenant)
	b := &announceBus{}
	e := NewEngine()
	e.SetStore(fs)
	if err := e.SetBus(b); err != nil {
		t.Fatalf("set bus: %v", err)
	}

	if err := e.Save(context.Background(), fenceIn(testTenant, "perimeter")); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := b.topics(); len(got) != 1 || got[0] != ReloadTopic {
		t.Fatalf("after a create the engine published %v, want one %s", got, ReloadTopic)
	}

	if _, err := e.Delete(context.Background(), testTenant, "perimeter"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := b.topics(); len(got) != 2 {
		t.Errorf("after a delete the engine published %v, want a second announcement", got)
	}
}

// The receiving side: an announcement arms the fence on a replica that knew
// nothing about it.
func TestAnAnnouncementArmsTheOtherReplica(t *testing.T) {
	fs := newFenceStore(testTenant)
	// The fence already exists in the database, written by the other replica.
	if err := fs.SaveGeofence(context.Background(), testTenant, &store.Geofence{
		ID: "perimeter", TenantID: testTenant, Name: "perimeter",
		Polygon: []store.GeoPoint{{Lat: 0, Lon: 0}, {Lat: 0, Lon: 1}, {Lat: 1, Lon: 1}, {Lat: 1, Lon: 0}},
		Trigger: "both", ChainID: "chain-" + testTenant, Enabled: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	b := &announceBus{}
	e := NewEngine()
	e.SetStore(fs)
	if err := e.SetBus(b); err != nil {
		t.Fatalf("set bus: %v", err)
	}
	if len(e.ListFences(testTenant)) != 0 {
		t.Fatal("precondition: this replica should know nothing yet")
	}

	payload, _ := json.Marshal(reloadEvent{TenantID: testTenant})
	_ = b.Publish(ReloadTopic, 1, false, payload)

	if got := e.ListFences(testTenant); len(got) != 1 {
		t.Errorf("after the announcement this replica has %v, want the fence", got)
	}
}
