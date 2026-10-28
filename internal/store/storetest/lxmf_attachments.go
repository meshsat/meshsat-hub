package storetest

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// testLXMFAttachments: a message's voice note and photo are listed in the
// message's order and read by index, each tenant sees its own only (the
// default tenant is the victim), an index is filed once, the total and the
// oldest-first listing serve eviction, and a delete names one row
// (MESHSAT-1735).
func testLXMFAttachments(t *testing.T, s store.Store) {
	ctx := context.Background()
	victim, other := store.DefaultTenantID, "att-other"
	base := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	mk := func(msg string, idx int, kind string, size int, at time.Time) *store.LXMFAttachment {
		a := &store.LXMFAttachment{MessageID: msg, Index: idx, Kind: kind, Size: size, ObjectKey: "lxmf/" + msg + "/" + strconv.Itoa(idx), CreatedAt: at}
		if kind == "audio" {
			a.Mode = 3
		} else {
			a.Format = "webp"
		}
		return a
	}
	// The messages the attachments belong to: the same id in both tenants.
	for _, row := range []struct{ tenant, id string }{{victim, "m1"}, {victim, "m2"}, {victim, "m3"}, {other, "m1"}} {
		if err := s.InsertLXMFMessage(ctx, row.tenant, &store.LXMFMessage{ID: row.id, Direction: store.LXMFDirectionIn, State: store.LXMFStateReceived,
			Source: "s", Destination: "d", Content: row.id, CreatedAt: base}); err != nil {
			t.Fatalf("message %s/%s: %v", row.tenant, row.id, err)
		}
	}
	for _, a := range []*store.LXMFAttachment{
		mk("m1", 0, "image", 300, base), mk("m1", 1, "audio", 200, base), mk("m2", 0, "audio", 100, base.Add(time.Minute)),
	} {
		if err := s.InsertLXMFAttachment(ctx, victim, a); err != nil || a.TenantID != victim {
			t.Fatalf("insert %s/%d: %v", a.MessageID, a.Index, err)
		}
	}
	if err := s.InsertLXMFAttachment(ctx, victim, mk("m1", 0, "image", 5, base)); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("duplicate index: %v", err)
	}
	// Another tenant may hold the same message id and index: keys carry the tenant.
	if err := s.InsertLXMFAttachment(ctx, other, mk("m1", 0, "audio", 50, base)); err != nil {
		t.Fatalf("same key in another tenant: %v", err)
	}

	// Every read of a message carries its count, this tenant's rows only.
	counts := func(tenant string) map[string]int {
		rows, err := s.ListLXMFMessages(ctx, tenant, store.LXMFQuery{})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int{}
		for _, r := range rows {
			out[r.ID] = r.Attachments
		}
		return out
	}
	if c := counts(victim); c["m1"] != 2 || c["m2"] != 1 || c["m3"] != 0 {
		t.Fatalf("the victim's counts: %v", c)
	}
	if c := counts(other); c["m1"] != 1 {
		t.Fatalf("the other tenant's count: %v", c)
	}
	if m1, err := s.GetLXMFMessage(ctx, victim, "m1"); err != nil || m1.Attachments != 2 {
		t.Fatalf("one message's count: %+v %v", m1, err)
	}

	list, err := s.ListLXMFAttachments(ctx, victim, "m1")
	if err != nil || len(list) != 2 || list[0].Index != 0 || list[0].Kind != "image" || list[0].Format != "webp" || list[0].Size != 300 ||
		list[1].Kind != "audio" || list[1].Mode != 3 || list[1].ObjectKey != "lxmf/m1/1" || !list[0].CreatedAt.Equal(base) {
		t.Fatalf("list m1: %+v %v", list, err)
	}
	if o, _ := s.ListLXMFAttachments(ctx, other, "m1"); len(o) != 1 || o[0].Kind != "audio" {
		t.Fatalf("other tenant's list: %+v", o)
	}
	if none, err := s.ListLXMFAttachments(ctx, "third", "m1"); err != nil || len(none) != 0 {
		t.Fatalf("third tenant: %+v %v", none, err)
	}
	got, err := s.GetLXMFAttachment(ctx, victim, "m1", 1)
	if err != nil || got.Kind != "audio" || got.Mode != 3 || got.Size != 200 || got.TenantID != victim {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := s.GetLXMFAttachment(ctx, victim, "m1", 2); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("index 2: %v", err)
	}
	if _, err := s.GetLXMFAttachment(ctx, other, "m2", 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other tenant read the victim's row: %v", err)
	}
	if n, _ := s.LXMFAttachmentBytes(ctx, victim); n != 600 {
		t.Fatalf("bytes %d", n)
	}
	if n, _ := s.LXMFAttachmentBytes(ctx, other); n != 50 {
		t.Fatalf("other bytes %d", n)
	}
	if n, _ := s.LXMFAttachmentBytes(ctx, "third"); n != 0 {
		t.Fatalf("third bytes %d", n)
	}

	oldest, err := s.OldestLXMFAttachments(ctx, victim, 2)
	if err != nil || len(oldest) != 2 || oldest[0].MessageID != "m1" || oldest[0].Index != 0 || oldest[1].Index != 1 {
		t.Fatalf("oldest: %+v %v", oldest, err)
	}
	if all, _ := s.OldestLXMFAttachments(ctx, victim, 0); len(all) != 3 || all[2].MessageID != "m2" {
		t.Fatalf("oldest all: %+v", all)
	}

	if err := s.DeleteLXMFAttachment(ctx, other, "m1", 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other tenant deleted the victim's row: %v", err)
	}
	if err := s.DeleteLXMFAttachment(ctx, victim, "m1", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteLXMFAttachment(ctx, victim, "m1", 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if list, _ := s.ListLXMFAttachments(ctx, victim, "m1"); len(list) != 1 || list[0].Index != 0 {
		t.Fatalf("after delete: %+v", list)
	}
	if c := counts(victim); c["m1"] != 1 {
		t.Fatalf("the count after the delete: %v", c)
	}
	if n, _ := s.LXMFAttachmentBytes(ctx, victim); n != 400 {
		t.Fatalf("bytes after delete %d", n)
	}
	if o, _ := s.ListLXMFAttachments(ctx, other, "m1"); len(o) != 1 {
		t.Fatal("delete touched another tenant")
	}
}
