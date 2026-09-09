package storetest

import (
	"context"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// testTenantQuota holds both stores to the same counting behaviour. The
// interesting part is what is NOT counted: a device row an integration made on
// its own is not something anybody bought, and billing a tenant for a busy TAK
// feed would charge them for traffic they did not create.
func testTenantQuota(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, id := range []string{"q-one", "q-two"} {
		if err := s.CreateTenant(ctx, &store.Tenant{ID: id, Slug: id, Name: id, Status: store.TenantActive}); err != nil {
			t.Fatalf("create tenant %s: %v", id, err)
		}
	}

	if n, err := s.CountBillableDevices(ctx, "q-one"); err != nil || n != 0 {
		t.Fatalf("empty tenant: %d, %v; want 0", n, err)
	}

	// Two the tenant provisioned, one the OTS poller mirrored.
	for _, d := range []store.Device{
		{IMEI: "q1-rock", Label: "rockblock", Type: "rockblock"},
		{IMEI: "q1-droid", Label: "android", Type: "android"},
		{IMEI: "ATAK-UID-0001", Label: "a marker", Type: "tak"},
	} {
		dev := d
		if err := s.CreateDevice(ctx, "q-one", &dev); err != nil {
			t.Fatalf("create device %s: %v", d.IMEI, err)
		}
	}
	if n, err := s.CountBillableDevices(ctx, "q-one"); err != nil || n != 2 {
		t.Errorf("billable devices = %d, %v; want 2 (the TAK marker must not count)", n, err)
	}

	// A device with no type set at all still counts: it is somebody's kit.
	untyped := store.Device{IMEI: "q1-untyped", Label: "no type"}
	if err := s.CreateDevice(ctx, "q-one", &untyped); err == nil {
		if n, _ := s.CountBillableDevices(ctx, "q-one"); n != 3 {
			t.Errorf("after an untyped device: %d, want 3", n)
		}
	}

	// Bridges share the ceiling and are counted separately.
	if n, err := s.CountBridges(ctx, "q-one"); err != nil || n != 0 {
		t.Fatalf("bridges before any: %d, %v; want 0", n, err)
	}
	if err := s.CreateOrUpdateBridge(ctx, "q-one", &store.Bridge{BridgeID: "q1-b1", Label: "b1"}); err != nil {
		t.Fatalf("create bridge: %v", err)
	}
	if n, err := s.CountBridges(ctx, "q-one"); err != nil || n != 1 {
		t.Errorf("bridges = %d, %v; want 1", n, err)
	}

	// A neighbour's rows never appear in this tenant's count.
	if err := s.CreateDevice(ctx, "q-two", &store.Device{IMEI: "q2-rock", Type: "rockblock"}); err != nil {
		t.Fatalf("neighbour device: %v", err)
	}
	if err := s.CreateOrUpdateBridge(ctx, "q-two", &store.Bridge{BridgeID: "q2-b1"}); err != nil {
		t.Fatalf("neighbour bridge: %v", err)
	}
	if n, _ := s.CountBillableDevices(ctx, "q-two"); n != 1 {
		t.Errorf("neighbour devices = %d, want 1", n)
	}
	if n, _ := s.CountBridges(ctx, "q-two"); n != 1 {
		t.Errorf("neighbour bridges = %d, want 1", n)
	}

	// Deleting frees the allowance again.
	before, _ := s.CountBillableDevices(ctx, "q-one")
	if err := s.DeleteDevice(ctx, "q-one", "q1-rock"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if after, _ := s.CountBillableDevices(ctx, "q-one"); after != before-1 {
		t.Errorf("after delete = %d, want %d", after, before-1)
	}
}
