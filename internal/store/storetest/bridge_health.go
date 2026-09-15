package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// A health report is one statement (MESHSAT-1155): it must leave the row
// exactly as the four separate updates did, including flipping a bridge the
// reaper had marked offline back to online.
func testBridgeHealth(t *testing.T, db store.Store) {
	ctx := context.Background()
	const tenant, other = "t_health", "t_health_other"
	for _, id := range []string{tenant, other} {
		if err := db.CreateTenant(ctx, &store.Tenant{ID: id, Slug: id, Name: id}); err != nil {
			t.Fatalf("create tenant %s: %v", id, err)
		}
		// Bridge ids are unique across tenants (an upsert on the id is how
		// the store treats them), so each tenant gets its own.
		if err := db.CreateOrUpdateBridge(ctx, id, &store.Bridge{BridgeID: "kit-" + id, Label: "kit"}); err != nil {
			t.Fatalf("create bridge for %s: %v", id, err)
		}
		if err := db.SetBridgeOnline(ctx, id, "kit-"+id, false); err != nil {
			t.Fatalf("set offline %s: %v", id, err)
		}
	}

	at := time.Date(2026, 9, 15, 13, 0, 0, 0, time.UTC)
	if err := db.RecordBridgeHealth(ctx, tenant, "kit-"+tenant, `{"protocol":1,"uptime":42}`, "mqtt", at); err != nil {
		t.Fatalf("record health: %v", err)
	}
	// Naming another tenant's bridge under this tenant writes nothing.
	if err := db.RecordBridgeHealth(ctx, tenant, "kit-"+other, `{"protocol":1}`, "mqtt", at); err != nil {
		t.Fatalf("record health across tenants: %v", err)
	}

	b, err := db.GetBridge(ctx, tenant, "kit-"+tenant)
	if err != nil {
		t.Fatalf("get bridge: %v", err)
	}
	if !b.Online {
		t.Error("a health report must put the bridge back online")
	}
	if b.LastHealth != `{"protocol":1,"uptime":42}` {
		t.Errorf("last_health = %q", b.LastHealth)
	}
	if b.LastSeen == nil || time.Since(*b.LastSeen) > time.Minute {
		t.Errorf("last_seen not touched: %v", b.LastSeen)
	}
	if b.LastReportBearer != "mqtt" || b.LastReportAt == nil || !b.LastReportAt.Equal(at) {
		t.Errorf("last report = %q %v, want mqtt %v", b.LastReportBearer, b.LastReportAt, at)
	}

	// The other tenant's bridge: untouched, still offline.
	o, err := db.GetBridge(ctx, other, "kit-"+other)
	if err != nil {
		t.Fatalf("get other: %v", err)
	}
	if o.Online || o.LastHealth != "" || o.LastReportBearer != "" {
		t.Errorf("the other tenant's bridge was written: online=%v health=%q bearer=%q", o.Online, o.LastHealth, o.LastReportBearer)
	}
}
