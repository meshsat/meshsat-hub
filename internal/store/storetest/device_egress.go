package storetest

import (
	"context"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// The satellite payload format a bridge declared for a modem (HeMB-NG S3-2):
// per registered modem and tenant, replaced only by another declaration, and
// independent of the bridge link and of the bridge's last birth.
func testDeviceEgress(t *testing.T, s store.Store) {
	ctx := context.Background()
	const tenant, other, imei = "t_egress", "t_egress_other", "300434060000077"
	for _, id := range []string{tenant, other} {
		if err := s.CreateTenant(ctx, &store.Tenant{ID: id, Slug: id, Name: id}); err != nil {
			t.Fatalf("create tenant %s: %v", id, err)
		}
	}
	// A modem is one tenant's (the IMEI is unique across tenants).
	if err := s.CreateDevice(ctx, tenant, &store.Device{IMEI: imei, Label: "9704"}); err != nil {
		t.Fatalf("create device: %v", err)
	}
	if by, format, err := s.DeviceEgress(ctx, tenant, imei); err != nil || by != "" || format != "" {
		t.Fatalf("before any declaration: %q %q %v", by, format, err)
	}
	if err := s.SetDeviceEgress(ctx, tenant, imei, "kit-a", "sealed"); err != nil {
		t.Fatal(err)
	}
	// The link moving, or the bridge's birth changing, leaves the record.
	if err := s.AssociateDeviceWithBridge(ctx, tenant, imei, "kit-b"); err != nil {
		t.Fatal(err)
	}
	if by, format, err := s.DeviceEgress(ctx, tenant, imei); err != nil || by != "kit-a" || format != "sealed" {
		t.Fatalf("after the link moved: %q %q %v", by, format, err)
	}
	if err := s.SetDeviceEgress(ctx, tenant, imei, "kit-b", "plain"); err != nil {
		t.Fatal(err)
	}
	if by, format, err := s.DeviceEgress(ctx, tenant, imei); err != nil || by != "kit-b" || format != "plain" {
		t.Fatalf("after a new declaration: %q %q %v", by, format, err)
	}
	// Another tenant can neither read nor write this tenant's modem's record.
	if by, format, err := s.DeviceEgress(ctx, other, imei); err != nil || by != "" || format != "" {
		t.Fatalf("another tenant reads the declaration: %q %q %v", by, format, err)
	}
	if err := s.SetDeviceEgress(ctx, other, imei, "kit-x", "sealed"); err != nil {
		t.Fatal(err)
	}
	if by, format, err := s.DeviceEgress(ctx, tenant, imei); err != nil || by != "kit-b" || format != "plain" {
		t.Fatalf("another tenant changed the declaration: %q %q %v", by, format, err)
	}
	// An unregistered modem keeps nothing and answers nothing.
	if err := s.SetDeviceEgress(ctx, tenant, "300434060000099", "kit-a", "sealed"); err != nil {
		t.Fatal(err)
	}
	if by, format, err := s.DeviceEgress(ctx, tenant, "300434060000099"); err != nil || by != "" || format != "" {
		t.Fatalf("unregistered modem: %q %q %v", by, format, err)
	}
}
