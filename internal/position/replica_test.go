package position

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// MESHSAT-1120. Both Hub replicas subscribe to meshsat/+/position with a plain
// Subscribe rather than a queue group, so both receive every message and both
// inserted a row. With a nanosecond clock for the id they produced two ids and
// two rows -- measured on production, one publish became two rows 210
// microseconds apart, and it had been true of every position from every device
// since the Hub went to two replicas.
//
// A real store rather than a fake: store.Store is a wide interface, and the
// property under test is that the DATABASE collapses the second insert, which a
// hand-written fake would simply assert into existence.

func testStore(t *testing.T) store.Store {
	t.Helper()
	db, err := sqlite.New(filepath.Join(t.TempDir(), "positions.db"), 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func positionPayloadJSON(lat, lon float64, at string) []byte {
	b, _ := json.Marshal(map[string]any{
		"lat": lat, "lon": lon, "source": "gps", "timestamp": at,
	})
	return b
}

// The headline: one message, two replicas, one row.
func TestTwoReplicasStoreOnePosition(t *testing.T) {
	db := testStore(t)
	const topic = "meshsat/dev-1/position"
	payload := positionPayloadJSON(52.37, 4.9, "2026-09-14T02:00:00Z")

	// Two subscribers over one database are two pods over one Postgres.
	for i := 0; i < 2; i++ {
		NewSubscriber(nil, db, nil).handlePosition(topic, payload)
	}

	got, err := db.ListPositions(context.Background(), store.DefaultTenantID, "dev-1", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("one published position produced %d rows, want 1. Every track on the "+
			"map is drawn from these, and the count scales with replicas.", len(got))
	}
	if got[0].Lat != 52.37 || got[0].Lon != 4.9 {
		t.Errorf("stored %+v", got[0])
	}
}

// A genuinely different position is still stored -- the id identifies the
// MESSAGE, so a device that moves gets a row per report.
func TestDistinctPositionsAreAllStored(t *testing.T) {
	db := testStore(t)
	const topic = "meshsat/dev-1/position"
	s := NewSubscriber(nil, db, nil)

	for i, p := range []struct {
		lat, lon float64
		at       string
	}{
		{52.37, 4.90, "2026-09-14T02:00:00Z"},
		{52.38, 4.90, "2026-09-14T02:01:00Z"}, // moved
		{52.38, 4.90, "2026-09-14T02:02:00Z"}, // same place, later: a real report
	} {
		payload := positionPayloadJSON(p.lat, p.lon, p.at)
		// Delivered to both replicas, as the broker does.
		s.handlePosition(topic, payload)
		NewSubscriber(nil, db, nil).handlePosition(topic, payload)
		_ = i
	}

	got, err := db.ListPositions(context.Background(), store.DefaultTenantID, "dev-1", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("three distinct reports produced %d rows, want 3. Deduping by message "+
			"must not swallow a device that keeps reporting.", len(got))
	}
}

// The id must come from the MESSAGE, not from a clock, or the two replicas
// never agree. This is the property the whole fix rests on.
func TestTheIDIsDerivedFromTheMessageNotTheClock(t *testing.T) {
	db := testStore(t)
	const topic = "meshsat/dev-1/position"
	payload := positionPayloadJSON(1, 2, "2026-09-14T02:00:00Z")

	NewSubscriber(nil, db, nil).handlePosition(topic, payload)
	time.Sleep(5 * time.Millisecond) // a clock-based id would differ by now
	NewSubscriber(nil, db, nil).handlePosition(topic, payload)

	got, _ := db.ListPositions(context.Background(), store.DefaultTenantID, "dev-1", 10)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1: the id is not stable across processes", len(got))
	}
	if got[0].ID == "" {
		t.Fatal("no id")
	}
	// Same message, same id, on any replica at any time.
	if want := hubmqtt.FallbackPositionID(topic, payload); got[0].ID != want {
		t.Errorf("id = %q, want the message digest %q", got[0].ID, want)
	}
}

// The dead man's switch check-in and the last_seen touch must still run on the
// replica whose insert was the duplicate. They are what tracks liveness, and an
// early return on a "failed" store would silence a device that is reporting
// perfectly well.
func TestTheDuplicateInsertStillTouchesLiveness(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	if err := db.CreateDevice(ctx, store.DefaultTenantID, &store.Device{
		IMEI: "dev-1", Label: "probe",
	}); err != nil {
		t.Fatalf("create device: %v", err)
	}

	const topic = "meshsat/dev-1/position"
	payload := positionPayloadJSON(52.37, 4.9, "2026-09-14T02:00:00Z")
	NewSubscriber(nil, db, nil).handlePosition(topic, payload)

	before, err := db.GetDevice(ctx, store.DefaultTenantID, "dev-1")
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if before.LastSeen.IsZero() {
		t.Fatal("the first insert did not touch last_seen")
	}

	// The SECOND replica: its insert collides and must still touch last_seen.
	if _, err := db.ListPositions(ctx, store.DefaultTenantID, "dev-1", 10); err != nil {
		t.Fatal(err)
	}
	NewSubscriber(nil, db, nil).handlePosition(topic, payload)
	after, err := db.GetDevice(ctx, store.DefaultTenantID, "dev-1")
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if after.LastSeen.IsZero() {
		t.Error("the replica whose insert was a duplicate stopped touching last_seen: " +
			"a device reporting normally would look silent")
	}
}

// Two DIFFERENT devices reporting identical coordinates must not collide. The
// topic is in the digest precisely for this: two kits parked side by side, or
// two devices whose payloads happen to match, are two devices.
func TestTwoDevicesWithTheSamePayloadDoNotCollide(t *testing.T) {
	db := testStore(t)
	payload := positionPayloadJSON(52.37, 4.9, "2026-09-14T02:00:00Z")

	for _, dev := range []string{"dev-a", "dev-b"} {
		topic := "meshsat/" + dev + "/position"
		// Both replicas, as ever.
		NewSubscriber(nil, db, nil).handlePosition(topic, payload)
		NewSubscriber(nil, db, nil).handlePosition(topic, payload)
	}

	ctx := context.Background()
	for _, dev := range []string{"dev-a", "dev-b"} {
		got, err := db.ListPositions(ctx, store.DefaultTenantID, dev, 10)
		if err != nil {
			t.Fatalf("list %s: %v", dev, err)
		}
		if len(got) != 1 {
			t.Errorf("%s has %d rows, want exactly 1. Without the topic in the digest, "+
				"two devices at the same coordinates are one row and one of them "+
				"vanishes from the map.", dev, len(got))
		}
	}
}
