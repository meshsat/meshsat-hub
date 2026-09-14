package geo

import (
	"context"
	"log/slog"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1119. Fences were held in a map and nothing wrote them down, so a
// customer's fence survived until the next rollout and the other replica never
// had it. Same shape, and the same fix, as the webhook dispatcher.

// Store is the slice of the persistence layer this package needs, declared
// here so internal/geo stays a geometry package with one narrow dependency.
type Store interface {
	ListTenants(ctx context.Context) ([]store.Tenant, error)
	ListGeofences(ctx context.Context, tenantID string) ([]store.Geofence, error)
	SaveGeofence(ctx context.Context, tenantID string, f *store.Geofence) error
	DeleteGeofence(ctx context.Context, tenantID string, id string) error
}

// SetStore attaches persistence. An engine with no store still works and
// forgets everything on restart, which is what this replaced.
func (e *Engine) SetStore(s Store) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.store = s
}

func (e *Engine) getStore() Store {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.store
}

func toStore(f Fence) store.Geofence {
	poly := make([]store.GeoPoint, 0, len(f.Polygon))
	for _, p := range f.Polygon {
		poly = append(poly, store.GeoPoint{Lat: p.Lat, Lon: p.Lon})
	}
	return store.Geofence{
		ID: f.ID, TenantID: f.TenantID, Name: f.Name, Polygon: poly,
		Trigger: string(f.Trigger), ChainID: f.ChainID, Enabled: f.Enabled,
	}
}

func fromStore(g store.Geofence) Fence {
	poly := make([]Point, 0, len(g.Polygon))
	for _, p := range g.Polygon {
		poly = append(poly, Point{Lat: p.Lat, Lon: p.Lon})
	}
	trigger := TriggerMode(g.Trigger)
	if trigger == "" {
		trigger = TriggerBoth
	}
	return Fence{
		ID: g.ID, TenantID: g.TenantID, Name: g.Name, Polygon: poly,
		Trigger: trigger, ChainID: g.ChainID, Enabled: g.Enabled,
	}
}

// LoadAll reads every tenant's fences into memory. Called once at startup.
//
// One unreadable tenant is logged and skipped rather than failing the whole
// load: a fence is a safety feature, and one bad row must not leave every other
// tenant's fences unarmed.
func (e *Engine) LoadAll(ctx context.Context) error {
	s := e.getStore()
	if s == nil {
		return nil
	}
	tenants, err := s.ListTenants(ctx)
	if err != nil {
		return err
	}
	n := 0
	for _, t := range tenants {
		rows, err := s.ListGeofences(ctx, t.ID)
		if err != nil {
			slog.Error("geofence: loading a tenant's fences", "tenant", t.ID, "error", err)
			continue
		}
		for _, r := range rows {
			e.AddFence(fromStore(r))
			n++
		}
	}
	slog.Info("geofence: loaded from the database", "fences", n, "tenants", len(tenants))
	return nil
}

// Save persists a fence and applies it here. The database write comes FIRST: a
// fence that is armed but not recorded disappears at the next rollout, and the
// customer has no way to tell that from the Hub having forgotten it on purpose.
func (e *Engine) Save(ctx context.Context, f Fence) error {
	if s := e.getStore(); s != nil {
		row := toStore(f)
		if err := s.SaveGeofence(ctx, f.TenantID, &row); err != nil {
			return err
		}
	}
	e.AddFence(f)
	return nil
}

// Delete removes one of the tenant's fences everywhere. It reports whether the
// engine held it.
func (e *Engine) Delete(ctx context.Context, tenantID, id string) (bool, error) {
	removed := e.RemoveFence(tenantID, id)
	if s := e.getStore(); s != nil {
		// Issued even when memory held nothing: this replica may not have
		// caught up with a create made on the other one.
		if err := s.DeleteGeofence(ctx, tenantID, id); err != nil && !isNotFound(err) {
			return removed, err
		}
	}
	return removed, nil
}

func isNotFound(err error) bool { return err == store.ErrNotFound }
