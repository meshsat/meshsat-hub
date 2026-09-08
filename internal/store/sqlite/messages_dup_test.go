package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// TestInsertMessageDuplicateIsNoOp: with stable message IDs a second replica
// (or a webhook retry) inserting the same message must yield ErrDuplicate and
// leave exactly one row.
func TestInsertMessageDuplicateIsNoOp(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := db.CreateDevice(ctx, testTenant, &store.Device{IMEI: "300434060000001", Label: "dup", Type: "rockblock"}); err != nil {
		t.Fatalf("create device: %v", err)
	}
	m := &store.Message{ID: "mo-300434060000001-7", DeviceIMEI: "300434060000001", Direction: "mo", Channel: "iridium", MOMSN: 7, Text: "hi", Status: "received"}
	if err := db.InsertMessage(ctx, testTenant, m); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	again := *m
	again.Text = "changed"
	err := db.InsertMessage(ctx, testTenant, &again)
	if !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("second insert: got %v, want ErrDuplicate", err)
	}
	msgs, err := db.ListMessages(ctx, testTenant, "300434060000001", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Text != "hi" {
		t.Fatalf("expected exactly one unchanged row, got %+v", msgs)
	}
}
