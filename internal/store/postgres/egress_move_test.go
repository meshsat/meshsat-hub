package postgres

import (
	"context"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Migration 44 moves the declarations r29 kept in the bridge row to
// bridge_egress with stamp 0, older than any birth, and empties the column.
// Its statements are run again here over r29 data, as a database at 43 meets
// them. [MESHSAT-1584]
func TestMigration44MovesR29Declarations(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	if err := db.CreateOrUpdateBridge(ctx, store.DefaultTenantID, &store.Bridge{BridgeID: "kit-r29", Label: "kit"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.rawDB.ExecContext(ctx,
		`UPDATE bridges SET egress_declarations='{"300434060000071":"sealed","300434060000072":"plain","300434060000073":"other"}'::jsonb WHERE bridge_id='kit-r29'`); err != nil {
		t.Fatal(err)
	}
	var m44 string
	for _, m := range migrations {
		if m.Version == 44 {
			m44 = m.SQL
		}
	}
	if m44 == "" {
		t.Fatal("no migration 44")
	}
	if _, err := db.rawDB.ExecContext(ctx, m44); err != nil {
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
	if err := db.rawDB.QueryRowContext(ctx, `SELECT egress_declarations::text FROM bridges WHERE bridge_id='kit-r29'`).Scan(&left); err != nil || left != "{}" {
		t.Fatalf("the r29 column after the move: %q %v", left, err)
	}
}
