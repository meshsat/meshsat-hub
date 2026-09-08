package sqlite

import (
	"context"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
)

func TestOOBPeerCRUD(t *testing.T) {
	db, err := New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	p := &store.OOBPeer{TenantID: "t1", BridgeID: "tesseract", PeerID: 38091, KeyEnc: []byte("enc"), LocalRole: 1, Phone: "+31653618463", SatIMEI: "300234065000001", Enabled: true}
	if err := db.UpsertOOBPeer(ctx, p); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := db.GetOOBPeer(ctx, "t1", "tesseract")
	if err != nil || got.PeerID != 38091 || got.Phone != p.Phone || string(got.KeyEnc) != "enc" || !got.Enabled {
		t.Fatalf("get: %+v %v", got, err)
	}
	for i := int64(1); i <= 3; i++ {
		n, err := db.NextOOBCounter(ctx, "t1", "tesseract")
		if err != nil || n != i {
			t.Fatalf("counter %d: %d %v", i, n, err)
		}
	}
	if _, err := db.NextOOBCounter(ctx, "t1", "nope"); err == nil {
		t.Fatalf("counter for an unknown peer succeeded")
	}
	if err := db.SetOOBReplayWindow(ctx, "t1", "tesseract", 7, 0b101); err != nil {
		t.Fatal(err)
	}
	list, _ := db.ListOOBPeersByPeerID(ctx, 38091)
	if len(list) != 1 || list[0].RxHigh != 7 || list[0].RxWindow != 5 || list[0].TxCounter != 3 {
		t.Fatalf("list: %+v", list)
	}
	p.Phone = "+31600000000"
	if err := db.UpsertOOBPeer(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetOOBPeer(ctx, "t1", "tesseract")
	if got.Phone != "+31600000000" {
		t.Fatalf("update: %+v", got)
	}
	if err := db.DeleteOOBPeer(ctx, "t1", "tesseract"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetOOBPeer(ctx, "t1", "tesseract"); err == nil {
		t.Fatalf("still present after delete")
	}
}
