package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// testTenantOffboarding holds both stores to the same promise: a tenant can be
// blocked, its data can be handed back, and it can be destroyed without taking
// anything of anyone else's with it.
func testTenantOffboarding(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now().UTC()
	mk := func(id string) {
		if err := s.CreateTenant(ctx, &store.Tenant{ID: id, Slug: id, Name: id, Plan: "beta", Status: store.TenantActive}); err != nil {
			t.Fatalf("create tenant %s: %v", id, err)
		}
	}
	mk("off-going")
	mk("off-staying")
	for _, tn := range []string{"off-going", "off-staying"} {
		if err := s.CreateDevice(ctx, tn, &store.Device{IMEI: "imei-" + tn, Label: tn}); err != nil {
			t.Fatalf("device %s: %v", tn, err)
		}
		if err := s.InsertMessage(ctx, tn, &store.Message{DeviceIMEI: "imei-" + tn, Direction: "MO", Status: "received"}); err != nil {
			t.Fatalf("message %s: %v", tn, err)
		}
	}

	// Export hands back this tenant's rows and nobody else's.
	exp, err := s.ExportTenant(ctx, "off-going")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(exp["devices"]) != 1 || len(exp["messages"]) != 1 {
		t.Errorf("export: devices=%d messages=%d, want 1 and 1", len(exp["devices"]), len(exp["messages"]))
	}
	if len(exp["tenants"]) != 1 {
		t.Errorf("export: tenant row missing")
	}
	for table, recs := range exp {
		if table == "tenants" {
			continue
		}
		for _, r := range recs {
			if got, ok := r["tenant_id"]; ok && got != "off-going" {
				t.Errorf("export leaked a %s row belonging to %v", table, got)
			}
		}
	}

	// Soft delete blocks but keeps the data.
	if err := s.SoftDeleteTenant(ctx, "off-going", now); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	got, err := s.GetTenant(ctx, "off-going")
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if got.Status != store.TenantDeleted {
		t.Errorf("status = %q, want %q", got.Status, store.TenantDeleted)
	}
	if got.DeletedAt == nil {
		t.Error("deleted_at not set")
	}
	if ds, err := s.ListDevices(ctx, "off-going"); err != nil || len(ds) != 1 {
		t.Errorf("soft delete destroyed data early: devices=%d err=%v", len(ds), err)
	}

	// The platform tenant is not closable.
	if err := s.SoftDeleteTenant(ctx, store.DefaultTenantID, now); err == nil {
		t.Error("soft delete of the default tenant was allowed")
	}

	// Only tenants past the grace period come due.
	due, err := s.ListTenantsDeletedBefore(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("list due: %v", err)
	}
	for _, d := range due {
		if d.ID == "off-going" {
			t.Error("a tenant deleted just now is already due for purge")
		}
	}
	due, err = s.ListTenantsDeletedBefore(ctx, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("list due: %v", err)
	}
	found := false
	for _, d := range due {
		if d.ID == "off-going" {
			found = true
		}
	}
	if !found {
		t.Error("a tenant past its grace period is not due for purge")
	}

	// Purge destroys everything of theirs and nothing of anyone else's.
	if err := s.PurgeTenant(ctx, "off-going"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, err := s.GetTenant(ctx, "off-going"); err == nil {
		t.Error("purged tenant still exists")
	}
	if ds, _ := s.ListDevices(ctx, "off-going"); len(ds) != 0 {
		t.Errorf("purge left %d devices behind", len(ds))
	}
	if ms, _ := s.ListMessages(ctx, "off-going", "", 100); len(ms) != 0 {
		t.Errorf("purge left %d messages behind", len(ms))
	}
	if ds, err := s.ListDevices(ctx, "off-staying"); err != nil || len(ds) != 1 {
		t.Errorf("purge took the neighbour's data: devices=%d err=%v", len(ds), err)
	}
	if ms, err := s.ListMessages(ctx, "off-staying", "", 100); err != nil || len(ms) != 1 {
		t.Errorf("purge took the neighbour's messages: %d err=%v", len(ms), err)
	}
	if err := s.PurgeTenant(ctx, store.DefaultTenantID); err == nil {
		t.Error("purge of the default tenant was allowed")
	}
}
