package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1117. MarkStaleBridgesOffline used one timeout for the whole
// platform. It now takes the platform DEFAULT and lets each tenant override it
// with tenants.bridge_offline_timeout, where 0 means "use the default".
//
// This lives in the conformance suite rather than in one store's own tests
// because the per-tenant lookup is written twice, in two dialects, and the
// Postgres one only runs under CI's test:postgres job. A test in the sqlite
// package would have proved the wrong half.
func testBridgeOfflineTimeout(t *testing.T, db store.Store) {
	ctx := context.Background()

	const (
		chooser  = "t_chooser"  // sets its own one-second timeout
		defaults = "t_defaults" // leaves it alone
	)
	for _, id := range []string{chooser, defaults} {
		if err := db.CreateTenant(ctx, &store.Tenant{ID: id, Slug: id, Name: id}); err != nil {
			t.Fatalf("create tenant %s: %v", id, err)
		}
		if err := db.CreateOrUpdateBridge(ctx, id, &store.Bridge{
			BridgeID: "bridge-" + id, Label: "b", Online: true,
		}); err != nil {
			t.Fatalf("create bridge for %s: %v", id, err)
		}
		if err := db.SetBridgeOnline(ctx, id, "bridge-"+id, true); err != nil {
			t.Fatalf("set online %s: %v", id, err)
		}
		if err := db.TouchBridgeLastSeen(ctx, id, "bridge-"+id); err != nil {
			t.Fatalf("touch %s: %v", id, err)
		}
	}

	tn, err := db.GetTenant(ctx, chooser)
	if err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	tn.BridgeOfflineTimeout = 1
	if err := db.UpdateTenant(ctx, tn); err != nil {
		t.Fatalf("update tenant: %v", err)
	}

	// SQLite stores last_seen at whole-second granularity, so two seconds is
	// the smallest wait that reliably puts the chooser's bridge past a
	// one-second timeout without putting it past the day-long default.
	time.Sleep(2100 * time.Millisecond)

	n, err := db.MarkStaleBridgesOffline(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("mark stale: %v", err)
	}
	if n != 1 {
		t.Errorf("marked %d bridges offline, want exactly 1", n)
	}

	got, err := db.GetBridge(ctx, chooser, "bridge-"+chooser)
	if err != nil {
		t.Fatalf("get chooser bridge: %v", err)
	}
	if got.Online {
		t.Error("the tenant set a one-second timeout and its bridge is still online after " +
			"two seconds of silence: the per-tenant value is not being read")
	}

	got, err = db.GetBridge(ctx, defaults, "bridge-"+defaults)
	if err != nil {
		t.Fatalf("get default-tenant bridge: %v", err)
	}
	if !got.Online {
		t.Error("a tenant that chose nothing had its bridge reaped on another tenant's " +
			"timeout rather than the platform default of a day")
	}
}

// A tenant row that does not exist must not make its bridges immortal. The
// obvious way to write the query -- UPDATE ... FROM tenants -- is an inner
// join, which silently skips exactly those rows.
func testBridgeOfflineTimeoutWithNoTenantRow(t *testing.T, db store.Store) {
	ctx := context.Background()
	const orphan = "t_orphan_no_row"

	if err := db.CreateOrUpdateBridge(ctx, orphan, &store.Bridge{
		BridgeID: "bridge-orphan", Label: "b", Online: true,
	}); err != nil {
		t.Fatalf("create bridge: %v", err)
	}
	if err := db.SetBridgeOnline(ctx, orphan, "bridge-orphan", true); err != nil {
		t.Fatalf("set online: %v", err)
	}
	if err := db.TouchBridgeLastSeen(ctx, orphan, "bridge-orphan"); err != nil {
		t.Fatalf("touch: %v", err)
	}

	time.Sleep(2100 * time.Millisecond)

	if _, err := db.MarkStaleBridgesOffline(ctx, time.Second); err != nil {
		t.Fatalf("mark stale: %v", err)
	}
	got, err := db.GetBridge(ctx, orphan, "bridge-orphan")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Online {
		t.Error("a bridge whose tenant has no row was never reaped. An inner join against " +
			"tenants leaves it online forever.")
	}
}
