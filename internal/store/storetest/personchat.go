package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1685, the "@" lane. In the conformance suite because the SQL is
// written twice, and because what it guards is money: the window and the
// ledger are what stop a person on the far end from spending a tenant's
// satellite credit. The default tenant is the VICTIM: a lookup that forgot
// its tenant collapses onto it, and a test with two customer tenants would
// pass by accident.
func testPersonChat(t *testing.T, db store.Store) {
	ctx := context.Background()
	const (
		mine  = "t_pc_mine"
		other = store.DefaultTenantID
		imei  = "300434060000001"
		peer  = "+31600000077"
	)
	now := time.Now().UTC().Truncate(time.Second)

	c := &store.PersonConversation{TenantID: mine, ID: "pc_a", DeviceID: imei, DeviceKind: "satellite",
		PeerKind: "sms", Peer: peer, Tag: "K7", Ref: "REFMINE234", LastOutAt: now.Add(-2 * time.Hour)}
	got, err := db.SavePersonConversationOut(ctx, c)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if got.Tag != "K7" || got.Ref != "REFMINE234" || !got.LastOutAt.Equal(c.LastOutAt) || got.LastInAt != nil {
		t.Fatalf("saved row reads back wrong: %+v", got)
	}

	// The victim tenant has a conversation with the same person, device id and ref.
	if _, err := db.SavePersonConversationOut(ctx, &store.PersonConversation{TenantID: other, ID: "pc_a", DeviceID: imei,
		DeviceKind: "satellite", PeerKind: "sms", Peer: peer, Tag: "M3", Ref: "REFMINE234", LastOutAt: now}); err != nil {
		t.Fatalf("the same id and ref in another tenant must be allowed: %v", err)
	}

	// Writing again moves last_out_at forward and keeps tag and ref: a reply
	// in flight still carries the old ones.
	got, err = db.SavePersonConversationOut(ctx, &store.PersonConversation{TenantID: mine, ID: "pc_a", DeviceID: imei,
		DeviceKind: "satellite", PeerKind: "sms", Peer: peer, Tag: "ZZ", Ref: "OTHERREF99", LastOutAt: now})
	if err != nil {
		t.Fatalf("save again: %v", err)
	}
	if got.Tag != "K7" || got.Ref != "REFMINE234" || !got.LastOutAt.Equal(now) {
		t.Fatalf("a second save must keep tag and ref and move last_out_at: %+v", got)
	}
	// ...and never backwards: a late write from the other replica is older.
	got, _ = db.SavePersonConversationOut(ctx, &store.PersonConversation{TenantID: mine, ID: "pc_a", DeviceID: imei,
		DeviceKind: "satellite", PeerKind: "sms", Peer: peer, Tag: "K7", Ref: "REFMINE234", LastOutAt: now.Add(-time.Hour)})
	if !got.LastOutAt.Equal(now) {
		t.Fatalf("last_out_at went backwards: %v", got.LastOutAt)
	}

	// The window: only conversations written in since `since`, only this tenant's.
	open, err := db.PersonConversationsWith(ctx, mine, "sms", peer, now.Add(-time.Minute))
	if err != nil || len(open) != 1 || open[0].TenantID != mine {
		t.Fatalf("open conversations = %+v, %v; want exactly mine", open, err)
	}
	if open, _ := db.PersonConversationsWith(ctx, mine, "sms", peer, now.Add(time.Minute)); len(open) != 0 {
		t.Fatalf("a conversation outside the window was returned: %+v", open)
	}
	if open, _ := db.PersonConversationsWith(ctx, mine, "email", peer, now.Add(-time.Hour)); len(open) != 0 {
		t.Fatal("an SMS conversation was returned for an email peer")
	}

	byRef, err := db.GetPersonConversationByRef(ctx, mine, "REFMINE234")
	if err != nil || byRef == nil || byRef.TenantID != mine || byRef.Tag != "K7" {
		t.Fatalf("by ref = %+v, %v", byRef, err)
	}
	if none, err := db.GetPersonConversationByRef(ctx, mine, "NOSUCHREF0"); err != nil || none != nil {
		t.Fatalf("an unknown ref must be (nil, nil), got %+v, %v", none, err)
	}

	in := now.Add(time.Minute)
	if err := db.MarkPersonConversationIn(ctx, mine, "pc_a", in, "<in-1@example.org>"); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkPersonConversationOut(ctx, mine, "pc_a", in, "<out-1@hub>"); err != nil {
		t.Fatal(err)
	}
	byRef, _ = db.GetPersonConversationByRef(ctx, mine, "REFMINE234")
	if byRef.LastInAt == nil || !byRef.LastInAt.Equal(in) || byRef.LastInMessageID != "<in-1@example.org>" ||
		byRef.LastOutMessageID != "<out-1@hub>" || !byRef.LastOutAt.Equal(in) {
		t.Fatalf("marks did not land: %+v", byRef)
	}
	// An older accepted send never moves the window back, and an empty
	// Message-ID keeps the one on file.
	if err := db.MarkPersonConversationOut(ctx, mine, "pc_a", now, ""); err != nil {
		t.Fatal(err)
	}
	byRef, _ = db.GetPersonConversationByRef(ctx, mine, "REFMINE234")
	if !byRef.LastOutAt.Equal(in) || byRef.LastOutMessageID != "<out-1@hub>" {
		t.Fatalf("an older mark moved the window back or lost the Message-ID: %+v", byRef)
	}
	victim, _ := db.GetPersonConversationByRef(ctx, other, "REFMINE234")
	if victim == nil || victim.LastInAt != nil || victim.LastOutMessageID != "" || victim.Tag != "M3" {
		t.Fatalf("marking my conversation touched the other tenant's: %+v", victim)
	}

	// A conversation saved for a send that then failed keeps the zero time:
	// a mistyped number opens no window.
	if _, err := db.SavePersonConversationOut(ctx, &store.PersonConversation{TenantID: mine, ID: "pc_new", DeviceID: imei,
		DeviceKind: "satellite", PeerKind: "sms", Peer: "+31600000078", Tag: "K7Q", Ref: "REFNEW2345"}); err != nil {
		t.Fatalf("save with no accepted send yet: %v", err)
	}
	if open, _ := db.PersonConversationsWith(ctx, mine, "sms", "+31600000078", now.Add(-24*time.Hour)); len(open) != 0 {
		t.Fatalf("a conversation with no accepted send is open: %+v", open)
	}
	if all, _ := db.PersonConversationsWith(ctx, mine, "sms", "+31600000078", time.Time{}); len(all) != 1 {
		t.Fatalf("the zero time must still list it for a caller asking for every conversation: %+v", all)
	}

	// The ledger: a repeated id is the same send, in each tenant.
	for _, s := range []store.PersonChatSend{
		{TenantID: mine, ID: "s1", ConversationID: "pc_a", Direction: "in", Bytes: 20, Status: "sent"},
		{TenantID: mine, ID: "s1", ConversationID: "pc_a", Direction: "in", Bytes: 20, Status: "sent"},
		{TenantID: other, ID: "s1", ConversationID: "pc_a", Direction: "in", E2E: true, Bytes: 5, Status: "sent"},
	} {
		s := s
		if err := db.RecordPersonChatSend(ctx, &s); err != nil {
			t.Fatalf("record %s/%s: %v", s.TenantID, s.ID, err)
		}
	}

	// The ceilings: max uses per (tenant, key), then refused, per tenant.
	for i := 0; i < 3; i++ {
		if ok, err := db.TakePersonChatSlot(ctx, mine, "dev:x:2026102412", 3); err != nil || !ok {
			t.Fatalf("slot %d: %v, %v", i, ok, err)
		}
	}
	if ok, err := db.TakePersonChatSlot(ctx, mine, "dev:x:2026102412", 3); err != nil || ok {
		t.Fatalf("the fourth use of a ceiling of 3 was allowed: %v, %v", ok, err)
	}
	if ok, _ := db.TakePersonChatSlot(ctx, other, "dev:x:2026102412", 3); !ok {
		t.Fatal("one tenant's ceiling counted against another's")
	}
	if ok, _ := db.TakePersonChatSlot(ctx, mine, "dev:x:2026102413", 3); !ok {
		t.Fatal("a new window must start at zero")
	}

	// The tenant's choice of mode round-trips.
	if err := db.CreateTenant(ctx, &store.Tenant{ID: mine, Slug: mine, Name: "pc", Plan: "free", Status: "active"}); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	ten, err := db.GetTenant(ctx, mine)
	if err != nil || ten == nil || ten.PersonChatMode != "" {
		t.Fatalf("a new tenant must carry the platform default (empty), got %+v, %v", ten, err)
	}
	ten.PersonChatMode = "contacts"
	if err := db.UpdateTenant(ctx, ten); err != nil {
		t.Fatal(err)
	}
	if ten, _ = db.GetTenant(ctx, mine); ten.PersonChatMode != "contacts" {
		t.Fatalf("person_chat_mode did not persist: %q", ten.PersonChatMode)
	}
}
