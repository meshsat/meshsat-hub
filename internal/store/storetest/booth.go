package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1175. The TTC booth relay, in the conformance suite because the SQL is
// written twice in two dialects and the Postgres half only runs under CI's
// test:postgres job -- and because the return leg is the part that has to be
// right in front of an audience.
func testBoothRelay(t *testing.T, db store.Store) {
	ctx := context.Background()
	const (
		mine   = "t_booth_mine"
		theirs = "t_booth_theirs"
		kit    = "nllei01parallax01"
		node   = "!a1b2c3d4"
	)
	future := time.Now().Add(30 * time.Minute)

	// A visitor with no history has no session. Not an error: that is simply
	// somebody who has not messaged us yet, and the caller starts one.
	got, err := db.GetBoothSession(ctx, mine, "+31600000001", "whatsapp")
	if err != nil {
		t.Fatalf("get absent session: %v", err)
	}
	if got != nil {
		t.Fatalf("an unknown visitor already had a session: %+v", got)
	}

	// Opt-in is a fact with a timestamp, not a field that follows the menu
	// position. Saving a later state that carries no opt-in must not erase it.
	optedIn := time.Now().UTC().Truncate(time.Second)
	if err := db.SaveBoothSession(ctx, &store.BoothSession{
		TenantID: mine, Sender: "+31600000001", Channel: "whatsapp",
		State: "awaiting_text", OptedInAt: &optedIn,
	}); err != nil {
		t.Fatalf("save session: %v", err)
	}
	if err := db.SaveBoothSession(ctx, &store.BoothSession{
		TenantID: mine, Sender: "+31600000001", Channel: "whatsapp", State: "menu",
	}); err != nil {
		t.Fatalf("resave session: %v", err)
	}
	got, err = db.GetBoothSession(ctx, mine, "+31600000001", "whatsapp")
	if err != nil || got == nil {
		t.Fatalf("get session: %v %+v", err, got)
	}
	if got.State != "menu" {
		t.Errorf("state = %q, want menu", got.State)
	}
	if got.OptedInAt == nil {
		t.Error("a later save cleared the recorded opt-in; consent is not a menu position")
	}

	// The same number on a different channel is a different conversation.
	if s, err := db.GetBoothSession(ctx, mine, "+31600000001", "sms"); err != nil || s != nil {
		t.Errorf("the WhatsApp session leaked onto the SMS channel: %v %+v", err, s)
	}

	// One relay, and the reply routes back to its sender by ref.
	r := &store.BoothRelay{
		TenantID: mine, Ref: "A7", Sender: "+31600000001", Channel: "whatsapp",
		BridgeID: kit, MeshDest: node, Body: "hello from the booth", ExpiresAt: future,
	}
	if err := db.CreateBoothRelay(ctx, r); err != nil {
		t.Fatalf("create relay: %v", err)
	}
	back, err := db.GetBoothRelayByRef(ctx, mine, "A7")
	if err != nil || back == nil {
		t.Fatalf("get by ref: %v %+v", err, back)
	}
	if back.Sender != "+31600000001" || back.BridgeID != kit {
		t.Errorf("ref A7 resolved to the wrong conversation: %+v", back)
	}
	if back.ClosedAt != nil {
		t.Error("a new relay is already closed")
	}

	// A ref is a tenant's own. Another tenant must not resolve it, or one
	// customer's reply reaches another customer's visitor.
	if other, err := db.GetBoothRelayByRef(ctx, theirs, "A7"); err != nil || other != nil {
		t.Errorf("ref A7 resolved across tenants: %v %+v", err, other)
	}

	// The fallback: one open conversation on this node means a reply with no
	// ref is still routable.
	open, err := db.OpenBoothRelaysFor(ctx, mine, kit, node)
	if err != nil {
		t.Fatalf("open relays: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("open conversations = %d, want 1", len(open))
	}

	// A second visitor on the same node is the normal booth case. Now a reply
	// with no ref is ambiguous, and the caller must ask rather than guess.
	if err := db.CreateBoothRelay(ctx, &store.BoothRelay{
		TenantID: mine, Ref: "B2", Sender: "+31600000002", Channel: "whatsapp",
		BridgeID: kit, MeshDest: node, Body: "second visitor", ExpiresAt: future,
	}); err != nil {
		t.Fatalf("create second relay: %v", err)
	}
	if open, err = db.OpenBoothRelaysFor(ctx, mine, kit, node); err != nil || len(open) != 2 {
		t.Fatalf("open conversations = %d (err %v), want 2 -- the ambiguity the fallback must refuse", len(open), err)
	}

	// Closing one disambiguates again.
	if err := db.CloseBoothRelay(ctx, mine, "A7"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if open, err = db.OpenBoothRelaysFor(ctx, mine, kit, node); err != nil || len(open) != 1 {
		t.Fatalf("after closing A7, open = %d (err %v), want 1", len(open), err)
	}
	if open[0].Ref != "B2" {
		t.Errorf("the wrong conversation stayed open: %q", open[0].Ref)
	}

	// Quotas count the relays themselves, so they cannot drift from what was
	// actually sent.
	since := time.Now().Add(-10 * time.Minute)
	if n, err := db.CountBoothRelaysBySender(ctx, mine, "+31600000001", since); err != nil || n != 1 {
		t.Errorf("per-sender count = %d (err %v), want 1", n, err)
	}
	if n, err := db.CountBoothRelays(ctx, mine, since); err != nil || n != 2 {
		t.Errorf("global count = %d (err %v), want 2", n, err)
	}
	// Another tenant's traffic must not spend this tenant's budget.
	if n, err := db.CountBoothRelays(ctx, theirs, since); err != nil || n != 0 {
		t.Errorf("another tenant's global count = %d (err %v), want 0", n, err)
	}
	// A window that begins after the relays counts none of them.
	if n, err := db.CountBoothRelays(ctx, mine, time.Now().Add(time.Minute)); err != nil || n != 0 {
		t.Errorf("future window count = %d (err %v), want 0", n, err)
	}

	// An expired conversation is not an open one, or a visitor from yesterday
	// silently receives today's reply.
	if err := db.CreateBoothRelay(ctx, &store.BoothRelay{
		TenantID: mine, Ref: "C9", Sender: "+31600000003", Channel: "whatsapp",
		BridgeID: kit, MeshDest: "!deadbeef", Body: "stale",
		ExpiresAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("create expired relay: %v", err)
	}
	if open, err = db.OpenBoothRelaysFor(ctx, mine, kit, "!deadbeef"); err != nil || len(open) != 0 {
		t.Errorf("an expired conversation counted as open: %d (err %v)", len(open), err)
	}
}
