package storetest

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// testLXMFHeld: a propagation node's store is per tenant (the default tenant
// is the victim), listing is per destination and smallest first, a delete
// touches only the named destination's rows, and pruning drops expired
// rows first and then the oldest until the byte cap holds (MESHSAT-1555 P5).
func testLXMFHeld(t *testing.T, s store.Store) {
	ctx := context.Background()
	victim, other := store.DefaultTenantID, "held-other"
	base := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	mk := func(id, dest string, size int, at time.Time) *store.LXMFHeldMessage {
		return &store.LXMFHeldMessage{ID: id, Destination: dest, Data: bytes.Repeat([]byte{0xab}, size), StampValue: 14, Source: "src", ReceivedAt: at}
	}
	for _, m := range []*store.LXMFHeldMessage{
		mk("t1", "d1", 300, base), mk("t2", "d1", 100, base.Add(time.Minute)), mk("t3", "d2", 200, base.Add(2*time.Minute)),
	} {
		if err := s.InsertLXMFHeld(ctx, victim, m); err != nil {
			t.Fatalf("insert %s: %v", m.ID, err)
		}
	}
	if err := s.InsertLXMFHeld(ctx, victim, mk("t1", "d1", 5, base)); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("duplicate transient id: %v", err)
	}
	if err := s.InsertLXMFHeld(ctx, other, mk("t1", "d1", 50, base)); err != nil {
		t.Fatalf("same id in another tenant: %v", err)
	}

	list, err := s.ListLXMFHeldFor(ctx, victim, "d1")
	if err != nil || len(list) != 2 || list[0].ID != "t2" || list[1].ID != "t1" || list[0].Size != 100 || len(list[0].Data) != 0 {
		t.Fatalf("list d1: %+v %v", list, err)
	}
	if l, _ := s.ListLXMFHeldFor(ctx, other, "d1"); len(l) != 1 || l[0].Size != 50 {
		t.Fatalf("other tenant's list: %+v", l)
	}
	got, err := s.GetLXMFHeld(ctx, victim, "t3")
	if err != nil || got.Destination != "d2" || len(got.Data) != 200 || got.StampValue != 14 || got.Source != "src" || !got.ReceivedAt.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := s.GetLXMFHeld(ctx, other, "t3"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other tenant read the victim's row: %v", err)
	}
	if n, _ := s.LXMFHeldBytes(ctx, victim); n != 600 {
		t.Fatalf("bytes %d", n)
	}

	// A delete names the destination: t3 is held for d2, so it stays.
	if n, err := s.DeleteLXMFHeld(ctx, victim, "d1", []string{"t2", "t3", "missing"}); err != nil || n != 1 {
		t.Fatalf("delete: %d %v", n, err)
	}
	if n, _ := s.LXMFHeldBytes(ctx, victim); n != 500 {
		t.Fatalf("bytes after delete %d", n)
	}
	if n, _ := s.LXMFHeldBytes(ctx, other); n != 50 {
		t.Fatal("delete touched another tenant")
	}

	// Pruning: nothing expired yet, cap 350 keeps the newest (t3, 200) and drops t1.
	if n, err := s.PruneLXMFHeld(ctx, victim, base.Add(-time.Hour), 350); err != nil || n != 1 {
		t.Fatalf("prune by bytes: %d %v", n, err)
	}
	if _, err := s.GetLXMFHeld(ctx, victim, "t1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("oldest row survived the cap")
	}
	if _, err := s.GetLXMFHeld(ctx, victim, "t3"); err != nil {
		t.Fatal("newest row was pruned")
	}
	// Expiry: everything before the cutoff goes.
	if n, err := s.PruneLXMFHeld(ctx, victim, base.Add(time.Hour), 0); err != nil || n != 1 {
		t.Fatalf("prune by age: %d %v", n, err)
	}
	if n, _ := s.LXMFHeldBytes(ctx, victim); n != 0 {
		t.Fatalf("bytes after expiry %d", n)
	}
	if n, _ := s.LXMFHeldBytes(ctx, other); n != 50 {
		t.Fatal("prune touched another tenant")
	}
}
