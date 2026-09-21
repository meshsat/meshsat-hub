package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// testBridgeIDCannotCrossTenants: a bridge id is unique across the platform,
// and a second tenant naming it once rewrote the first tenant's row through
// the upsert (MESHSAT-1307). The default tenant is the victim, per the
// tenancy tests' convention.
func testBridgeIDCannotCrossTenants(t *testing.T, db store.Store) {
	ctx := context.Background()
	if err := db.CreateOrUpdateBridge(ctx, store.DefaultTenantID, &store.Bridge{BridgeID: "kit-owned", Label: "victim"}); err != nil {
		t.Fatalf("victim create: %v", err)
	}
	err := db.CreateOrUpdateBridge(ctx, "t-intruder", &store.Bridge{BridgeID: "kit-owned", Label: "intruder", ReticulumHash: "ffff"})
	if !errors.Is(err, store.ErrOwnedElsewhere) {
		t.Fatalf("another tenant's upsert on the id: %v, want ErrOwnedElsewhere", err)
	}
	b, err := db.GetBridge(ctx, store.DefaultTenantID, "kit-owned")
	if err != nil || b.Label != "victim" || b.ReticulumHash != "" {
		t.Fatalf("the victim's row was rewritten: %+v %v", b, err)
	}
	if b, _ := db.GetBridge(ctx, "t-intruder", "kit-owned"); b != nil {
		t.Fatalf("the intruder got a row: %+v", b)
	}
	// The owner updating its own bridge still works.
	if err := db.CreateOrUpdateBridge(ctx, store.DefaultTenantID, &store.Bridge{BridgeID: "kit-owned", Label: "renamed"}); err != nil {
		t.Fatalf("the owner's own update: %v", err)
	}
	if b, _ := db.GetBridge(ctx, store.DefaultTenantID, "kit-owned"); b == nil || b.Label != "renamed" {
		t.Fatalf("owner update not applied: %+v", b)
	}
}
