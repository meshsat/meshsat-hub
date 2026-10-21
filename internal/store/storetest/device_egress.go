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

// What a bridge declared per modem, merged by the statement that saves its
// birth (HeMB-NG S3-2, r29): a birth without a modem keeps the earlier entry,
// a later declaration replaces it, a retry merges, and another tenant's
// bridge is untouched.
func testBridgeEgress(t *testing.T, s store.Store) {
	ctx := context.Background()
	const tenant, other = "t_begress", "t_begress_other"
	const a, b = "300434060000081", "300434060000082"
	for _, id := range []string{tenant, other} {
		if err := s.CreateTenant(ctx, &store.Tenant{ID: id, Slug: id, Name: id}); err != nil {
			t.Fatalf("create tenant %s: %v", id, err)
		}
	}
	get := func(tn, bridge, imei string) string {
		t.Helper()
		f, err := s.BridgeEgress(ctx, tn, bridge, imei)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	if got := get(tenant, "kit-begress", a); got != "" {
		t.Fatalf("unknown bridge: %q", got)
	}
	save := func(decl string) {
		t.Helper()
		if err := s.CreateOrUpdateBridge(ctx, tenant, &store.Bridge{BridgeID: "kit-begress", Label: "kit", EgressDeclarations: decl}); err != nil {
			t.Fatal(err)
		}
	}
	save(`{"` + a + `":"sealed"}`)
	if got := get(tenant, "kit-begress", a); got != "sealed" {
		t.Fatalf("first birth: %q", got)
	}
	save("") // a birth that declares nothing
	if got := get(tenant, "kit-begress", a); got != "sealed" {
		t.Fatalf("a birth without the modem erased it: %q", got)
	}
	save(`{"` + b + `":"plain"}`)
	if got, gotB := get(tenant, "kit-begress", a), get(tenant, "kit-begress", b); got != "sealed" || gotB != "plain" {
		t.Fatalf("a birth naming another modem: %q %q", got, gotB)
	}
	save(`{"` + a + `":"plain"}`)
	if got := get(tenant, "kit-begress", a); got != "plain" {
		t.Fatalf("explicit plain: %q", got)
	}
	if err := s.MergeBridgeEgress(ctx, tenant, "kit-begress", map[string]string{a: "sealed"}); err != nil {
		t.Fatal(err)
	}
	if got, gotB := get(tenant, "kit-begress", a), get(tenant, "kit-begress", b); got != "sealed" || gotB != "plain" {
		t.Fatalf("retry merge: %q %q", got, gotB)
	}
	if got := get(other, "kit-begress", a); got != "" {
		t.Fatalf("another tenant reads the bridge's declarations: %q", got)
	}
	if err := s.MergeBridgeEgress(ctx, other, "kit-begress", map[string]string{a: "plain"}); err != nil {
		t.Fatal(err)
	}
	if got := get(tenant, "kit-begress", a); got != "sealed" {
		t.Fatalf("another tenant changed the bridge's declarations: %q", got)
	}
}
