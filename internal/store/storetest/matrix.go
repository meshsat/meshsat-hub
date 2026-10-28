package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1736, the Matrix bridge. In the conformance suite because the SQL is
// written twice, and because an operator read in the wrong tenant would post
// one tenant's crew's answer into another tenant's room. The default tenant is
// the VICTIM.
func testMatrixBridge(t *testing.T, db store.Store) {
	ctx := context.Background()
	const (
		mine  = "t_mx_mine"
		other = store.DefaultTenantID
		op    = "mx0123456789abcdef"
	)
	now := time.Now().UTC().Truncate(time.Second)

	if o, err := db.GetMatrixOperator(ctx, mine, op); err != nil || o != nil {
		t.Fatalf("an unknown operator: %+v %v", o, err)
	}
	if err := db.UpsertMatrixOperator(ctx, &store.MatrixOperator{TenantID: mine, UserID: op,
		RoomID: "!incident:example.org", MatrixUserID: "@anna:example.org", DisplayName: "Anna", LastUsedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertMatrixOperator(ctx, &store.MatrixOperator{TenantID: other, UserID: op,
		RoomID: "!victim:example.org", MatrixUserID: "@victim:example.org", DisplayName: "Victim", LastUsedAt: now}); err != nil {
		t.Fatal(err)
	}
	// An older use updates the names but never moves the time back.
	if err := db.UpsertMatrixOperator(ctx, &store.MatrixOperator{TenantID: mine, UserID: op,
		RoomID: "!incident:example.org", MatrixUserID: "@anna:example.org", DisplayName: "Anna B", LastUsedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	o, err := db.GetMatrixOperator(ctx, mine, op)
	if err != nil || o == nil {
		t.Fatalf("get: %+v %v", o, err)
	}
	if o.RoomID != "!incident:example.org" || o.MatrixUserID != "@anna:example.org" || o.DisplayName != "Anna B" {
		t.Fatalf("read another tenant's operator or lost the update: %+v", o)
	}
	if d := o.LastUsedAt.Sub(now); d < -time.Second || d > time.Second {
		t.Fatalf("last used moved: %v, want %v", o.LastUsedAt, now)
	}

	if s, err := db.GetMatrixSync(ctx, mine); err != nil || s != nil {
		t.Fatalf("an unknown sync state: %+v %v", s, err)
	}
	if err := db.SaveMatrixSync(ctx, &store.MatrixSync{TenantID: mine, BotUserID: "@bot:example.org", NextBatch: "s1"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMatrixSync(ctx, &store.MatrixSync{TenantID: other, BotUserID: "@victim-bot:example.org", NextBatch: "v9"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMatrixSync(ctx, &store.MatrixSync{TenantID: mine, BotUserID: "@bot:example.org", NextBatch: "s2"}); err != nil {
		t.Fatal(err)
	}
	s, err := db.GetMatrixSync(ctx, mine)
	if err != nil || s == nil || s.NextBatch != "s2" || s.BotUserID != "@bot:example.org" {
		t.Fatalf("sync state: %+v %v", s, err)
	}
	if v, _ := db.GetMatrixSync(ctx, other); v == nil || v.NextBatch != "v9" {
		t.Fatalf("the victim's sync state was touched: %+v", v)
	}
}
