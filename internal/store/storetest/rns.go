package storetest

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// testRNSPaths: the Reticulum path table is keyed by destination hash, an
// upsert replaces the row, binary columns round-trip byte for byte, and a
// delete removes exactly one destination (MESHSAT-1555).
func testRNSPaths(t *testing.T, s store.Store) {
	ctx := context.Background()
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	a := &store.RNSPath{DestHash: "0123456789abcdef0123456789abcdef", NextHop: "0123456789abcdef0123456789abcdef", Hops: 1,
		Iface: "tcpb_7", ExpiresAt: exp, RandomBlobs: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, AnnounceRaw: []byte{0x08, 0x01, 0xff, 0x00, 0x7e}}
	b := &store.RNSPath{DestHash: "fedcba9876543210fedcba9876543210", NextHop: "0123456789abcdef0123456789abcdef", Hops: 2,
		Iface: "tcpb_7", ExpiresAt: exp, AnnounceRaw: []byte{0x09}}
	for _, p := range []*store.RNSPath{a, b} {
		if err := s.UpsertRNSPath(ctx, p); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	list, err := s.ListRNSPaths(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v %+v", err, list)
	}
	got := list[0]
	if got.DestHash != a.DestHash || got.NextHop != a.NextHop || got.Hops != 1 || got.Iface != "tcpb_7" ||
		!got.ExpiresAt.Equal(exp) || !bytes.Equal(got.RandomBlobs, a.RandomBlobs) || !bytes.Equal(got.AnnounceRaw, a.AnnounceRaw) || got.UpdatedAt.IsZero() {
		t.Fatalf("row a round trip: %+v", got)
	}
	if list[1].RandomBlobs != nil && len(list[1].RandomBlobs) != 0 {
		t.Fatalf("row b had no blobs, got %v", list[1].RandomBlobs)
	}

	// A newer announce for the same destination replaces the row, with its
	// new hop count and both random blobs kept.
	a.Hops, a.Iface = 3, "mqtt_rns"
	a.RandomBlobs = append(a.RandomBlobs, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20)
	if err := s.UpsertRNSPath(ctx, a); err != nil {
		t.Fatalf("upsert again: %v", err)
	}
	list, _ = s.ListRNSPaths(ctx)
	if len(list) != 2 || list[0].Hops != 3 || list[0].Iface != "mqtt_rns" || len(list[0].RandomBlobs) != 20 {
		t.Fatalf("after replace: %+v", list)
	}

	if err := s.DeleteRNSPath(ctx, b.DestHash); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteRNSPath(ctx, "not-a-known-destination"); err != nil {
		t.Fatalf("delete unknown must be a no-op: %v", err)
	}
	list, _ = s.ListRNSPaths(ctx)
	if len(list) != 1 || list[0].DestHash != a.DestHash {
		t.Fatalf("after delete: %+v", list)
	}
}
