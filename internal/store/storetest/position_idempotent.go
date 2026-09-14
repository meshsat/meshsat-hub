package storetest

import (
	"context"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1120. Both replicas insert the same position, with an id derived from
// the MQTT message so the second insert is meant to collapse. That only works if
// the store makes it a silent no-op: an error would make the caller treat it as
// a failed store and return early, skipping the last_seen touch, the dead man's
// switch check-in and the geofence evaluation that come after it.
//
// In the conformance suite because it is written twice, in two dialects
// (ON CONFLICT DO NOTHING and INSERT OR IGNORE), and the Postgres one only runs
// under CI's test:postgres job.
func testPositionInsertIsIdempotent(t *testing.T, db store.Store) {
	ctx := context.Background()
	const tenant = "t_pos_idem"

	p := &store.Position{
		ID: "pos-deadbeefdeadbeef", DeviceIMEI: "dev-idem",
		Lat: 52.37, Lon: 4.9, Source: "gps",
	}
	if err := db.InsertPosition(ctx, tenant, p); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// The other replica, same message, same id.
	again := *p
	if err := db.InsertPosition(ctx, tenant, &again); err != nil {
		t.Fatalf("the duplicate insert returned an error: %v.\n"+
			"It must be a silent no-op -- the caller reads an error as a failed store "+
			"and returns before touching last_seen or the dead man's switch.", err)
	}

	got, err := db.ListPositions(ctx, tenant, "dev-idem", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("two inserts of one message produced %d rows, want 1", len(got))
	}

	// A different message still lands.
	other := *p
	other.ID = "pos-cafebabecafebabe"
	other.Lat = 52.38
	if err := db.InsertPosition(ctx, tenant, &other); err != nil {
		t.Fatalf("second distinct insert: %v", err)
	}
	got, _ = db.ListPositions(ctx, tenant, "dev-idem", 10)
	if len(got) != 2 {
		t.Errorf("a distinct position produced %d rows total, want 2", len(got))
	}
}
