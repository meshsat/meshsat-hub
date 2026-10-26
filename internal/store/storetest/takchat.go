package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1706, TAK direct messages. In the conformance suite because the SQL
// is written twice, and because a contact read in the wrong tenant would send
// one tenant's dispatcher's words to another tenant's kit. The default tenant
// is the VICTIM.
func testTAKChat(t *testing.T, db store.Store) {
	ctx := context.Background()
	const (
		mine  = "t_tak_mine"
		other = store.DefaultTenantID
		crew  = "meshsat-a1b3c3a4"
	)
	now := time.Now().UTC().Truncate(time.Second)

	if c, err := db.GetTAKContact(ctx, mine, crew); err != nil || c != nil {
		t.Fatalf("an unknown contact: %+v %v", c, err)
	}
	if err := db.UpsertTAKContact(ctx, &store.TAKContact{TenantID: mine, UID: crew, Callsign: "MESHSAT-c3a4",
		BridgeID: "parallax01", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	// The victim has the same uid under another bridge.
	if err := db.UpsertTAKContact(ctx, &store.TAKContact{TenantID: other, UID: crew, Callsign: "VICTIM",
		BridgeID: "victimkit", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	// An older sighting moves the owner but never the time back.
	if err := db.UpsertTAKContact(ctx, &store.TAKContact{TenantID: mine, UID: crew, Callsign: "MESHSAT-c3a4",
		BridgeID: "tesseract01", LastSeen: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	c, err := db.GetTAKContact(ctx, mine, crew)
	if err != nil || c == nil {
		t.Fatalf("get: %+v %v", c, err)
	}
	if c.BridgeID != "tesseract01" || c.Callsign != "MESHSAT-c3a4" || !c.LastSeen.Equal(now) {
		t.Errorf("contact %+v", c)
	}
	list, err := db.ListTAKContacts(ctx, mine, now.Add(-time.Minute))
	if err != nil || len(list) != 1 || list[0].UID != crew || list[0].BridgeID != "tesseract01" {
		t.Fatalf("list %+v %v", list, err)
	}
	if list, _ := db.ListTAKContacts(ctx, mine, now.Add(time.Minute)); len(list) != 0 {
		t.Errorf("a contact not seen since was listed: %+v", list)
	}
	if v, _ := db.GetTAKContact(ctx, other, crew); v == nil || v.BridgeID != "victimkit" {
		t.Errorf("the victim's contact changed: %+v", v)
	}

	// Messages: one row per id, per tenant; oldest first; the last limit.
	for i, m := range []store.TAKChatMessage{
		{TenantID: mine, ID: "m1", CrewUID: crew, OperatorUID: "meshsat-hub-op-7", UserID: "7", Direction: "out",
			Text: "check in", BridgeID: "tesseract01", Status: "sent", CreatedAt: now.Add(-2 * time.Minute)},
		{TenantID: mine, ID: "m2", CrewUID: crew, OperatorUID: "meshsat-hub-op-7", Direction: "in",
			Text: "on my way", BridgeID: "tesseract01", Status: "received", CreatedAt: now.Add(-time.Minute)},
		{TenantID: other, ID: "m1", CrewUID: crew, OperatorUID: "meshsat-hub-op-9", Direction: "out",
			Text: "victim's", CreatedAt: now},
	} {
		created, err := db.SaveTAKChatMessage(ctx, &m)
		if err != nil || !created {
			t.Fatalf("save %d: %v %v", i, created, err)
		}
	}
	again := store.TAKChatMessage{TenantID: mine, ID: "m2", CrewUID: crew, OperatorUID: "meshsat-hub-op-7",
		Direction: "in", Text: "on my way", CreatedAt: now}
	if created, err := db.SaveTAKChatMessage(ctx, &again); err != nil || created {
		t.Errorf("the same answer stored twice: %v %v", created, err)
	}
	// A status set later is the row's, and only in its own tenant.
	if err := db.SetTAKChatMessageStatus(ctx, mine, "m1", "failed"); err != nil {
		t.Fatal(err)
	}
	msgs, err := db.ListTAKChatMessages(ctx, mine, crew, 10)
	if err != nil || len(msgs) != 2 || msgs[0].ID != "m1" || msgs[1].ID != "m2" {
		t.Fatalf("messages %+v %v", msgs, err)
	}
	if msgs[0].Text != "check in" || msgs[0].UserID != "7" || msgs[0].Status != "failed" || msgs[1].Direction != "in" {
		t.Errorf("messages read back wrong: %+v", msgs)
	}
	if last, _ := db.ListTAKChatMessages(ctx, mine, crew, 1); len(last) != 1 || last[0].ID != "m2" {
		t.Errorf("limit 1 gave %+v, want the newest", last)
	}
	if v, _ := db.ListTAKChatMessages(ctx, other, crew, 10); len(v) != 1 || v[0].Text != "victim's" {
		t.Errorf("the victim's thread: %+v", v)
	}
}
