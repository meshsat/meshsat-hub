package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// testLXMFMessages: a tenant's inbox is its own (the default tenant is the
// victim), the sender's state machine holds, listing filters and pages, and
// pruning keeps the newest rows and never a message still being sent
// (MESHSAT-1555).
func testLXMFMessages(t *testing.T, s store.Store) {
	ctx := context.Background()
	victim, other := store.DefaultTenantID, "lxmf-other"
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

	in := &store.LXMFMessage{Hash: "aa", Direction: store.LXMFDirectionIn, State: store.LXMFStateReceived, Source: "s1", Destination: "d1",
		Title: "hello", Content: "to the victim", Method: "opportunistic", StampValue: 7, SignatureValid: true, MessageAt: base, CreatedAt: base}
	if err := s.InsertLXMFMessage(ctx, victim, in); err != nil || in.ID == "" {
		t.Fatalf("insert: %v", err)
	}
	if err := s.InsertLXMFMessage(ctx, victim, &store.LXMFMessage{ID: in.ID, Direction: "in", State: "received", Source: "x", Destination: "y"}); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("duplicate id: %v", err)
	}
	// Another tenant may reuse the id: keys are (tenant_id, id).
	if err := s.InsertLXMFMessage(ctx, other, &store.LXMFMessage{ID: in.ID, Direction: "in", State: "received", Source: "x", Destination: "y", Content: "other's"}); err != nil {
		t.Fatalf("same id in another tenant: %v", err)
	}

	got, err := s.GetLXMFMessage(ctx, victim, in.ID)
	if err != nil || got.Content != "to the victim" || got.StampValue != 7 || !got.SignatureValid || !got.MessageAt.Equal(base) || got.Method != "opportunistic" {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if o, _ := s.GetLXMFMessage(ctx, other, in.ID); o.Content != "other's" {
		t.Fatalf("other tenant's row: %+v", o)
	}
	if _, err := s.GetLXMFMessage(ctx, "third", in.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("third tenant read a row: %v", err)
	}

	// Outbound state machine.
	out := &store.LXMFMessage{Direction: store.LXMFDirectionOut, State: store.LXMFStateQueued, Source: "d1", Destination: "peer", Content: "reply", CreatedByEmail: "op@example.org", CreatedAt: base.Add(time.Minute)}
	if err := s.InsertLXMFMessage(ctx, victim, out); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateLXMFMessageState(ctx, other, out.ID, store.LXMFStateUpdate{State: store.LXMFStateFailed}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other tenant moved the victim's message: %v", err)
	}
	if err := s.UpdateLXMFMessageState(ctx, victim, out.ID, store.LXMFStateUpdate{State: store.LXMFStateSending}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateLXMFMessageState(ctx, victim, out.ID, store.LXMFStateUpdate{State: store.LXMFStateDelivered, Hash: "bb", Method: "direct"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetLXMFMessage(ctx, victim, out.ID)
	if got.State != store.LXMFStateDelivered || got.Hash != "bb" || got.Method != "direct" || got.Detail != "" || got.CreatedByEmail != "op@example.org" {
		t.Fatalf("after delivery: %+v", got)
	}
	if err := s.UpdateLXMFMessageState(ctx, victim, out.ID, store.LXMFStateUpdate{State: store.LXMFStateFailed, Detail: "no path"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetLXMFMessage(ctx, victim, out.ID)
	if got.Hash != "bb" || got.Method != "direct" || got.Detail != "no path" {
		t.Fatalf("empty update fields must keep the stored values: %+v", got)
	}

	// Listing: newest first, filters, paging; the other tenant's rows never appear.
	for i := 0; i < 5; i++ {
		m := &store.LXMFMessage{Direction: store.LXMFDirectionIn, State: store.LXMFStateReceived, Source: "s", Destination: "d1", Content: fmt.Sprint("n", i), CreatedAt: base.Add(time.Duration(i+2) * time.Minute)}
		if err := s.InsertLXMFMessage(ctx, victim, m); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.ListLXMFMessages(ctx, victim, store.LXMFQuery{})
	if err != nil || len(all) != 7 || all[0].Content != "n4" || all[6].Content != "to the victim" {
		t.Fatalf("list all: %d %v", len(all), err)
	}
	if ins, _ := s.ListLXMFMessages(ctx, victim, store.LXMFQuery{Direction: store.LXMFDirectionIn}); len(ins) != 6 {
		t.Fatalf("list in: %d", len(ins))
	}
	if failed, _ := s.ListLXMFMessages(ctx, victim, store.LXMFQuery{State: store.LXMFStateFailed}); len(failed) != 1 || failed[0].ID != out.ID {
		t.Fatalf("list failed: %+v", failed)
	}
	if page, _ := s.ListLXMFMessages(ctx, victim, store.LXMFQuery{Limit: 2, Before: base.Add(4 * time.Minute)}); len(page) != 2 || page[0].Content != "n1" {
		t.Fatalf("page: %+v", page)
	}
	if n, _ := s.CountLXMFMessages(ctx, victim); n != 7 {
		t.Fatalf("count %d", n)
	}
	if n, _ := s.CountLXMFMessages(ctx, other); n != 1 {
		t.Fatalf("other count %d", n)
	}

	// Pruning keeps the newest, skips a message still going out.
	q := &store.LXMFMessage{Direction: store.LXMFDirectionOut, State: store.LXMFStateQueued, Source: "d1", Destination: "peer", Content: "still queued", CreatedAt: base.Add(-time.Hour)}
	if err := s.InsertLXMFMessage(ctx, victim, q); err != nil {
		t.Fatal(err)
	}
	// Eight rows, three kept, the queued one spared: four go.
	gone, err := s.PruneLXMFMessages(ctx, victim, 3)
	if err != nil || gone != 4 {
		t.Fatalf("prune: gone %d err %v", gone, err)
	}
	left, _ := s.ListLXMFMessages(ctx, victim, store.LXMFQuery{})
	if len(left) != 4 || left[0].Content != "n4" || left[3].ID != q.ID {
		t.Fatalf("after prune: %+v", left)
	}
	if n, _ := s.CountLXMFMessages(ctx, other); n != 1 {
		t.Fatal("prune touched another tenant")
	}
}
