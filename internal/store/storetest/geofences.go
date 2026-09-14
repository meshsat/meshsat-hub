package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1119. Geofences had no table at all: a fence lived in one replica's
// memory until the next rollout. In the conformance suite because the SQL is
// written twice, in two dialects, and the Postgres one only runs under CI's
// test:postgres job.
func testGeofences(t *testing.T, db store.Store) {
	ctx := context.Background()
	const (
		mine   = "t_fence_mine"
		theirs = "t_fence_theirs"
	)

	square := []store.GeoPoint{{Lat: 0, Lon: 0}, {Lat: 0, Lon: 1}, {Lat: 1, Lon: 1}}
	f := store.Geofence{
		ID: "perimeter", Name: "Perimeter", Polygon: square,
		Trigger: "both", ChainID: "chain-1", Enabled: true, CooldownSec: 600,
	}
	if err := db.SaveGeofence(ctx, mine, &f); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := db.ListGeofences(ctx, mine)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("listed %d fences, want 1", len(got))
	}
	if got[0].ChainID != "chain-1" {
		t.Errorf("chain_id = %q, want chain-1. The chain is what makes a crossing page "+
			"somebody; losing it in the round trip is losing the alert.", got[0].ChainID)
	}
	if len(got[0].Polygon) != 3 || got[0].Polygon[2].Lon != 1 {
		t.Errorf("polygon did not survive the round trip: %v", got[0].Polygon)
	}
	if !got[0].Enabled || got[0].Trigger != "both" {
		t.Errorf("enabled/trigger did not survive: %+v", got[0])
	}
	if got[0].CooldownSec != 600 {
		t.Errorf("cooldown_sec = %d, want 600. A fence that loses its cooldown falls back "+
			"to the platform default silently, and the owner's choice did nothing.",
			got[0].CooldownSec)
	}

	// Two tenants may name a fence the same thing. The key is (tenant_id, id),
	// not id -- the defect webhook_configs actually had, where an upsert on a
	// shared id let one tenant take over another's row.
	other := store.Geofence{
		ID: "perimeter", Name: "Their Perimeter",
		Polygon: []store.GeoPoint{{Lat: 9, Lon: 9}, {Lat: 9, Lon: 10}, {Lat: 10, Lon: 10}},
		Trigger: "enter", ChainID: "chain-2", Enabled: true,
	}
	if err := db.SaveGeofence(ctx, theirs, &other); err != nil {
		t.Fatalf("save for the other tenant: %v", err)
	}
	mineAgain, _ := db.ListGeofences(ctx, mine)
	if len(mineAgain) != 1 || mineAgain[0].Name != "Perimeter" || mineAgain[0].ChainID != "chain-1" {
		t.Fatalf("the second tenant's fence overwrote the first's: %+v", mineAgain)
	}

	// An update of the caller's own fence replaces it rather than duplicating.
	f.Name = "Renamed"
	if err := db.SaveGeofence(ctx, mine, &f); err != nil {
		t.Fatalf("update: %v", err)
	}
	mineAgain, _ = db.ListGeofences(ctx, mine)
	if len(mineAgain) != 1 || mineAgain[0].Name != "Renamed" {
		t.Fatalf("after an update: %+v, want one fence named Renamed", mineAgain)
	}

	// A delete is tenant-scoped.
	if err := db.DeleteGeofence(ctx, theirs, "perimeter"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if left, _ := db.ListGeofences(ctx, mine); len(left) != 1 {
		t.Error("deleting one tenant's fence removed another tenant's")
	}
	if err := db.DeleteGeofence(ctx, theirs, "perimeter"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleting a fence that is gone returned %v, want ErrNotFound", err)
	}
}
