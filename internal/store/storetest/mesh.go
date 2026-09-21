package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1181: mesh node presence, in the conformance suite because the SQL is
// written twice and the GREATEST/MAX clause is the part that differs between the
// dialects while having to behave identically.
func testMeshPresence(t *testing.T, db store.Store) {
	ctx := context.Background()
	const (
		mine     = "t_mesh_mine"
		theirs   = "t_mesh_theirs"
		kitA     = "bridge-kit-a"
		kitB     = "bridge-kit-b"
		nodeOne  = "!0a0b0c0d"
		nodeTwo  = "!0a0b0c1a"
		nodeRoam = "!f0ccc009"
	)
	now := time.Now().UTC().Truncate(time.Second)

	// A bridge nobody has been heard behind yields nothing. That is the normal
	// state at the start of a show day, and it is NOT an error.
	got, err := db.MeshNodesSeenSince(ctx, mine, kitA, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("empty lookup: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("an unheard bridge already had nodes: %+v", got)
	}

	if err := db.RecordMeshNode(ctx, mine, kitA, nodeOne, now.Add(-2*time.Minute)); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := db.RecordMeshNode(ctx, mine, kitA, nodeTwo, now.Add(-90*time.Minute)); err != nil {
		t.Fatalf("record stale: %v", err)
	}

	// The window is the whole point: a node heard 90 minutes ago is not evidence
	// that anything is listening now.
	got, err = db.MeshNodesSeenSince(ctx, mine, kitA, now.Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("windowed lookup: %v", err)
	}
	if len(got) != 1 || got[0].NodeID != nodeOne {
		t.Fatalf("window returned %+v, want only the recent node %s", got, nodeOne)
	}
	if got[0].LastHeard.IsZero() || got[0].FirstHeard.IsZero() {
		t.Errorf("timestamps did not survive the round trip: %+v", got[0])
	}
	// Widen it and the older node is there too.
	if got, err = db.MeshNodesSeenSince(ctx, mine, kitA, now.Add(-3*time.Hour)); err != nil || len(got) != 2 {
		t.Errorf("wide window returned %d nodes (err %v), want 2", len(got), err)
	}

	// Presence is per BRIDGE, not per node. A node in range of two kits is two
	// true facts; recording it behind kitB must not move it away from kitA.
	if err := db.RecordMeshNode(ctx, mine, kitB, nodeOne, now.Add(-time.Minute)); err != nil {
		t.Fatalf("record on second bridge: %v", err)
	}
	if got, err = db.MeshNodesSeenSince(ctx, mine, kitA, now.Add(-30*time.Minute)); err != nil || len(got) != 1 {
		t.Errorf("kitA lost its node when the same node was heard behind kitB: %d (err %v)", len(got), err)
	}
	if got, err = db.MeshNodesSeenSince(ctx, mine, kitB, now.Add(-30*time.Minute)); err != nil || len(got) != 1 {
		t.Errorf("kitB did not record the node: %d (err %v)", len(got), err)
	}

	// Another tenant's mesh is not this tenant's mesh.
	if err := db.RecordMeshNode(ctx, theirs, kitA, nodeRoam, now); err != nil {
		t.Fatalf("record other tenant: %v", err)
	}
	if got, err = db.MeshNodesSeenSince(ctx, mine, kitA, now.Add(-30*time.Minute)); err != nil || len(got) != 1 {
		t.Errorf("another tenant's node appeared on this tenant's mesh: %+v (err %v)", got, err)
	}

	// last_heard must never go backwards. Both replicas see every mesh message
	// and may write in either order; a late-arriving older timestamp that
	// overwrote a newer one would make a live mesh look stale and take the kit
	// out of the menu.
	before, err := db.MeshNodesSeenSince(ctx, mine, kitA, now.Add(-30*time.Minute))
	if err != nil || len(before) != 1 {
		t.Fatalf("pre-reorder read: %d (err %v)", len(before), err)
	}
	if err := db.RecordMeshNode(ctx, mine, kitA, nodeOne, now.Add(-45*time.Minute)); err != nil {
		t.Fatalf("out-of-order record: %v", err)
	}
	after, err := db.MeshNodesSeenSince(ctx, mine, kitA, now.Add(-30*time.Minute))
	if err != nil || len(after) != 1 {
		t.Fatalf("an out-of-order write moved last_heard backwards and the node left the window: %d (err %v)",
			len(after), err)
	}
	if !after[0].LastHeard.Equal(before[0].LastHeard) {
		t.Errorf("last_heard moved backwards: was %s, now %s", before[0].LastHeard, after[0].LastHeard)
	}

	// A newer write does advance it.
	if err := db.RecordMeshNode(ctx, mine, kitA, nodeOne, now); err != nil {
		t.Fatalf("advancing record: %v", err)
	}
	advanced, err := db.MeshNodesSeenSince(ctx, mine, kitA, now.Add(-30*time.Second))
	if err != nil || len(advanced) != 1 {
		t.Fatalf("a newer write did not advance last_heard: %d (err %v)", len(advanced), err)
	}
}
