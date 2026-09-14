package geo

import (
	"context"
	"sync"
	"testing"

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
	fresh.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5)
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
