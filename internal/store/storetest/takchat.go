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
		crew  = "meshsat-0a0b0c0d"
	)
	now := time.Now().UTC().Truncate(time.Second)

	if c, err := db.GetTAKContact(ctx, mine, crew); err != nil || c != nil {
		t.Fatalf("an unknown contact: %+v %v", c, err)
	}
	if err := db.UpsertTAKContact(ctx, &store.TAKContact{TenantID: mine, UID: crew, Callsign: "MESHSAT-0c0d",
		BridgeID: "parallax01", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	// The victim has the same uid under another bridge.
	if err := db.UpsertTAKContact(ctx, &store.TAKContact{TenantID: other, UID: crew, Callsign: "VICTIM",
		BridgeID: "victimkit", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	// An older sighting moves the owner but never the time back.
	if err := db.UpsertTAKContact(ctx, &store.TAKContact{TenantID: mine, UID: crew, Callsign: "MESHSAT-0c0d",
		BridgeID: "tesseract01", LastSeen: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	c, err := db.GetTAKContact(ctx, mine, crew)
	if err != nil || c == nil {
		t.Fatalf("get: %+v %v", c, err)
	}
	if c.BridgeID != "tesseract01" || c.Callsign != "MESHSAT-0c0d" || !c.LastSeen.Equal(now) {
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
	if err := db.SetTAKChatMessageStatus(ctx, mine, "m1", "failed", "the bus refused it"); err != nil {
		t.Fatal(err)
	}
	msgs, err := db.ListTAKChatMessages(ctx, mine, crew, 10)
	if err != nil || len(msgs) != 2 || msgs[0].ID != "m1" || msgs[1].ID != "m2" {
		t.Fatalf("messages %+v %v", msgs, err)
	}
	if msgs[0].Text != "check in" || msgs[0].UserID != "7" || msgs[0].Status != "failed" ||
		msgs[0].StatusDetail != "the bus refused it" || msgs[1].Direction != "in" {
		t.Errorf("messages read back wrong: %+v", msgs)
	}
	if last, _ := db.ListTAKChatMessages(ctx, mine, crew, 1); len(last) != 1 || last[0].ID != "m2" {
		t.Errorf("limit 1 gave %+v, want the newest", last)
	}
	if v, _ := db.ListTAKChatMessages(ctx, other, crew, 10); len(v) != 1 || v[0].Text != "victim's" {
		t.Errorf("the victim's thread: %+v", v)
	}

	// MESHSAT-1746: the bridge's word on a message it was handed. Only that
	// bridge, only an "out" row, and a "queued" never over a final word.
	m3 := store.TAKChatMessage{TenantID: mine, ID: "m3", CrewUID: crew, OperatorUID: "meshsat-hub-op-mx01", UserID: "mx01",
		Direction: "out", Text: "where are you?", BridgeID: "tesseract01", Status: "sent", ClientRef: "matrix:$e1", CreatedAt: now}
	if created, err := db.SaveTAKChatMessage(ctx, &m3); err != nil || !created {
		t.Fatalf("save m3: %v %v", created, err)
	}
	if got, err := db.SetTAKChatMessageOutcome(ctx, mine, "parallax01", "m3", "undelivered", "another kit's word"); err != nil || got != nil {
		t.Errorf("a bridge that was not handed the message set its status: %+v %v", got, err)
	}
	if got, err := db.SetTAKChatMessageOutcome(ctx, mine, "tesseract01", "m2", "undelivered", "an answer"); err != nil || got != nil {
		t.Errorf("a status landed on a crew's answer: %+v %v", got, err)
	}
	if got, err := db.SetTAKChatMessageOutcome(ctx, other, "tesseract01", "m3", "undelivered", "wrong tenant"); err != nil || got != nil {
		t.Errorf("a status crossed tenants: %+v %v", got, err)
	}
	got, err := db.SetTAKChatMessageOutcome(ctx, mine, "tesseract01", "m3", "queued", "queued as a direct message")
	if err != nil || got == nil || got.Status != "queued" || got.StatusDetail != "queued as a direct message" ||
		got.ClientRef != "matrix:$e1" || got.UserID != "mx01" || got.CrewUID != crew || got.Direction != "out" {
		t.Fatalf("queued: %+v %v", got, err)
	}
	got, err = db.SetTAKChatMessageOutcome(ctx, mine, "tesseract01", "m3", "undelivered", "the kit has not heard it for a day")
	if err != nil || got == nil || got.Status != "undelivered" || got.StatusDetail != "the kit has not heard it for a day" {
		t.Fatalf("undelivered: %+v %v", got, err)
	}
	// The two reports arrived the wrong way round: the final word stands.
	if got, err := db.SetTAKChatMessageOutcome(ctx, mine, "tesseract01", "m3", "queued", "late"); err != nil || got != nil {
		t.Errorf("a late queued undid a final word: %+v %v", got, err)
	}
	// A final word may follow a final word (failed after queued elsewhere), and
	// the same word again is the same row.
	if got, err := db.SetTAKChatMessageOutcome(ctx, mine, "tesseract01", "m3", "undelivered", "the kit has not heard it for a day"); err != nil || got == nil {
		t.Errorf("the same word again: %+v %v", got, err)
	}
	msgs, _ = db.ListTAKChatMessages(ctx, mine, crew, 10)
	if len(msgs) != 3 || msgs[2].ID != "m3" || msgs[2].Status != "undelivered" || msgs[2].ClientRef != "matrix:$e1" ||
		msgs[2].StatusDetail != "the kit has not heard it for a day" {
		t.Errorf("m3 read back: %+v", msgs)
	}
	if msgs[0].Status != "failed" || msgs[1].Status != "received" {
		t.Errorf("other rows changed: %+v", msgs)
	}

	// MESHSAT-1744: the bearer a message went on is kept and read back by
	// id, in its own tenant only.
	m4 := store.TAKChatMessage{TenantID: mine, ID: "m4", CrewUID: crew, OperatorUID: "meshsat-hub-op-7", UserID: "7",
		Direction: "out", Text: "over sms", BridgeID: "tesseract01", Status: "sent", Bearer: "sms", CreatedAt: now}
	if created, err := db.SaveTAKChatMessage(ctx, &m4); err != nil || !created {
		t.Fatalf("save m4: %v %v", created, err)
	}
	got, err = db.GetTAKChatMessage(ctx, mine, "m4")
	if err != nil || got == nil || got.Bearer != "sms" || got.Text != "over sms" || got.BridgeID != "tesseract01" || got.UserID != "7" {
		t.Fatalf("get m4: %+v %v", got, err)
	}
	if got, err := db.GetTAKChatMessage(ctx, other, "m4"); err != nil || got != nil {
		t.Errorf("m4 read across tenants: %+v %v", got, err)
	}
	if got, err := db.GetTAKChatMessage(ctx, mine, "nope"); err != nil || got != nil {
		t.Errorf("an unknown id: %+v %v", got, err)
	}
	if got, _ := db.SetTAKChatMessageOutcome(ctx, mine, "tesseract01", "m4", "undelivered", "not heard"); got == nil || got.Bearer != "sms" {
		t.Errorf("the bearer did not survive an outcome: %+v", got)
	}
	msgs, _ = db.ListTAKChatMessages(ctx, mine, crew, 10)
	if len(msgs) != 4 || msgs[3].Bearer != "sms" || msgs[0].Bearer != "" {
		t.Errorf("bearers listed: %+v", msgs)
	}
}
