package sqlite

import (
	"context"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// The declarations r29 kept in the bridge row move to bridge_egress with
// stamp 0, older than any birth, and the column is emptied, so a later start
// does not copy them over a newer declaration. [MESHSAT-1584]
func TestR29DeclarationsMoveToTheirTable(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	if err := db.CreateOrUpdateBridge(ctx, store.DefaultTenantID, &store.Bridge{BridgeID: "kit-r29", Label: "kit"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx,
		`UPDATE bridges SET egress_declarations='{"300434060000071":"sealed","300434060000072":"plain","300434060000073":"other"}' WHERE bridge_id='kit-r29'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil { // the next start
		t.Fatal(err)
	}
	for imei, want := range map[string]store.EgressDecl{
		"300434060000071": {Format: "sealed"},
		"300434060000072": {Format: "plain"},
		"300434060000073": {},
	} {
		if got, err := db.BridgeEgressEntry(ctx, store.DefaultTenantID, "kit-r29", imei); err != nil || got != want {
			t.Fatalf("%s: %+v %v, want %+v", imei, got, err, want)
		}
	}
	var left string
	if err := db.db.QueryRowContext(ctx, `SELECT egress_declarations FROM bridges WHERE bridge_id='kit-r29'`).Scan(&left); err != nil || left != "{}" {
		t.Fatalf("the r29 column after the move: %q %v", left, err)
	}
	// A newer birth's declaration is not overwritten by a later start.
	if err := db.CreateOrUpdateBridge(ctx, store.DefaultTenantID, &store.Bridge{BridgeID: "kit-r29", Label: "kit", BirthStamp: 5,
		Egress: map[string]store.EgressDecl{"300434060000071": {Format: "plain", Stamp: 5}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.BridgeEgress(ctx, store.DefaultTenantID, "kit-r29", "300434060000071"); got != "plain" {
		t.Fatalf("a start after a newer birth: %q", got)
	}
}
