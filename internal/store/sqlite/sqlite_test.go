package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cubeos-app/meshsat-hub/internal/store"
)

const testTenant = "test-tenant"

func testDB(t *testing.T) *DB {
	t.Helper()
	db, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestDeviceCRUD(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// Create
	dev := &store.Device{IMEI: "300234063904190", Label: "Field Unit 1", Type: "rockblock"}
	if err := db.CreateDevice(ctx, testTenant, dev); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Get
	got, err := db.GetDevice(ctx, testTenant, "300234063904190")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Label != "Field Unit 1" {
		t.Errorf("label: got %q", got.Label)
	}

	// List
	all, err := db.ListDevices(ctx, testTenant)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("list: got %d, want 1", len(all))
	}

	// Update
	dev.Label = "Updated"
	if err := db.UpdateDevice(ctx, testTenant, dev); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, _ := db.GetDevice(ctx, testTenant, "300234063904190")
	if got2.Label != "Updated" {
		t.Errorf("updated label: got %q", got2.Label)
	}

	// TouchLastSeen
	if err := db.TouchDeviceLastSeen(ctx, testTenant, "300234063904190"); err != nil {
		t.Fatalf("touch: %v", err)
	}

	// Delete
	if err := db.DeleteDevice(ctx, testTenant, "300234063904190"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	all2, _ := db.ListDevices(ctx, testTenant)
	if len(all2) != 0 {
		t.Errorf("after delete: got %d", len(all2))
	}
}

func TestMessageCRUD(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	_ = db.CreateDevice(ctx, testTenant, &store.Device{IMEI: "300234063904190", Label: "Test", Type: "rockblock"})

	msg := &store.Message{
		DeviceIMEI: "300234063904190",
		Direction:  "mo",
		Channel:    "iridium",
		MOMSN:      42,
		Text:       "Hello from field",
		Status:     "received",
	}
	if err := db.InsertMessage(ctx, testTenant, msg); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if msg.ID == "" {
		t.Error("expected auto-generated ID")
	}

	msgs, err := db.ListMessages(ctx, testTenant, "300234063904190", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("list: got %d", len(msgs))
	}
	if msgs[0].Text != "Hello from field" {
		t.Errorf("text: got %q", msgs[0].Text)
	}

	got, err := db.GetMessage(ctx, testTenant, msg.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MOMSN != 42 {
		t.Errorf("momsn: got %d", got.MOMSN)
	}
}

func TestWebhookCRUD(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	wh := &store.WebhookConfig{
		ID:      "wh-1",
		URL:     "https://example.com/hook",
		Secret:  "s3cret",
		Events:  []string{"mo", "sos"},
		Enabled: true,
	}
	if err := db.SaveWebhook(ctx, testTenant, wh); err != nil {
		t.Fatalf("save: %v", err)
	}

	all, err := db.ListWebhooks(ctx, testTenant)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("list: got %d", len(all))
	}
	if all[0].URL != "https://example.com/hook" {
		t.Errorf("url: got %q", all[0].URL)
	}
	if len(all[0].Events) != 2 {
		t.Errorf("events: got %v", all[0].Events)
	}

	if err := db.DeleteWebhook(ctx, testTenant, "wh-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	all2, _ := db.ListWebhooks(ctx, testTenant)
	if len(all2) != 0 {
		t.Errorf("after delete: got %d", len(all2))
	}
}

func TestPositions(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	_ = db.CreateDevice(ctx, testTenant, &store.Device{IMEI: "300234063904190", Label: "Test", Type: "rockblock"})

	p1 := &store.Position{DeviceIMEI: "300234063904190", Lat: 52.3676, Lon: 4.9041, Source: "iridium_cep", CEP: 10.0}
	if err := db.InsertPosition(ctx, testTenant, p1); err != nil {
		t.Fatalf("insert p1: %v", err)
	}
	// SQLite datetime('now') has 1s resolution — use rowid ordering via sequential IDs
	p2 := &store.Position{DeviceIMEI: "300234063904190", Lat: 52.3700, Lon: 4.9100, Source: "gps"}
	if err := db.InsertPosition(ctx, testTenant, p2); err != nil {
		t.Fatalf("insert p2: %v", err)
	}

	latest, err := db.LatestPosition(ctx, testTenant, "300234063904190")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest.Source != "gps" {
		t.Errorf("latest source: got %q, want gps", latest.Source)
	}

	all, _ := db.ListPositions(ctx, testTenant, "300234063904190", 10)
	if len(all) != 2 {
		t.Errorf("list: got %d", len(all))
	}
}

func TestAuditLog(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	_ = db.InsertAuditEntry(ctx, testTenant, &store.AuditEntry{Action: "login", Actor: "admin", IP: "1.2.3.4"})
	_ = db.InsertAuditEntry(ctx, testTenant, &store.AuditEntry{Action: "device.create", Actor: "admin", Detail: "IMEI=300234063904190"})

	entries, err := db.ListAuditEntries(ctx, testTenant, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("list: got %d", len(entries))
	}
}

func TestTenantIsolation(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	tenantA := "tenant-alpha"
	tenantB := "tenant-beta"

	// Create devices in different tenants with same IMEI (allowed since tenant_id differentiates).
	devA := &store.Device{IMEI: "111111111111111", Label: "Alpha Device", Type: "rockblock"}
	devB := &store.Device{IMEI: "222222222222222", Label: "Beta Device", Type: "astrocast"}
	if err := db.CreateDevice(ctx, tenantA, devA); err != nil {
		t.Fatalf("create A: %v", err)
	}
	if err := db.CreateDevice(ctx, tenantB, devB); err != nil {
		t.Fatalf("create B: %v", err)
	}

	// Tenant A should only see its device.
	devicesA, _ := db.ListDevices(ctx, tenantA)
	if len(devicesA) != 1 {
		t.Fatalf("tenant A devices: got %d, want 1", len(devicesA))
	}
	if devicesA[0].Label != "Alpha Device" {
		t.Errorf("tenant A device label: got %q", devicesA[0].Label)
	}

	// Tenant B should only see its device.
	devicesB, _ := db.ListDevices(ctx, tenantB)
	if len(devicesB) != 1 {
		t.Fatalf("tenant B devices: got %d, want 1", len(devicesB))
	}
	if devicesB[0].Label != "Beta Device" {
		t.Errorf("tenant B device label: got %q", devicesB[0].Label)
	}

	// Tenant A cannot get tenant B's device.
	_, err := db.GetDevice(ctx, tenantA, "222222222222222")
	if err == nil {
		t.Error("tenant A should NOT be able to get tenant B's device")
	}

	// Tenant B cannot delete tenant A's device.
	_ = db.DeleteDevice(ctx, tenantB, "111111111111111")
	gotA, err := db.GetDevice(ctx, tenantA, "111111111111111")
	if err != nil || gotA == nil {
		t.Error("tenant A device should still exist after tenant B delete attempt")
	}

	// Insert messages in each tenant, verify isolation.
	_ = db.InsertMessage(ctx, tenantA, &store.Message{DeviceIMEI: "111111111111111", Direction: "mo", Text: "alpha msg", Status: "received"})
	_ = db.InsertMessage(ctx, tenantB, &store.Message{DeviceIMEI: "222222222222222", Direction: "mo", Text: "beta msg", Status: "received"})

	msgsA, _ := db.ListMessages(ctx, tenantA, "", 100)
	if len(msgsA) != 1 || msgsA[0].Text != "alpha msg" {
		t.Errorf("tenant A messages: got %d, want 1 with 'alpha msg'", len(msgsA))
	}
	msgsB, _ := db.ListMessages(ctx, tenantB, "", 100)
	if len(msgsB) != 1 || msgsB[0].Text != "beta msg" {
		t.Errorf("tenant B messages: got %d, want 1 with 'beta msg'", len(msgsB))
	}

	// Positions isolation.
	_ = db.InsertPosition(ctx, tenantA, &store.Position{DeviceIMEI: "111111111111111", Lat: 1.0, Lon: 2.0, Source: "gps"})
	_ = db.InsertPosition(ctx, tenantB, &store.Position{DeviceIMEI: "222222222222222", Lat: 3.0, Lon: 4.0, Source: "gps"})

	posA, err := db.LatestPosition(ctx, tenantA, "111111111111111")
	if err != nil || posA.Lat != 1.0 {
		t.Error("tenant A position mismatch")
	}
	_, err = db.LatestPosition(ctx, tenantA, "222222222222222")
	if err == nil {
		t.Error("tenant A should NOT see tenant B's position")
	}

	// Audit log isolation.
	_ = db.InsertAuditEntry(ctx, tenantA, &store.AuditEntry{Action: "login", Actor: "admin-a"})
	_ = db.InsertAuditEntry(ctx, tenantB, &store.AuditEntry{Action: "login", Actor: "admin-b"})

	auditA, _ := db.ListAuditEntries(ctx, tenantA, 100)
	if len(auditA) != 1 || auditA[0].Actor != "admin-a" {
		t.Errorf("tenant A audit: got %d entries", len(auditA))
	}
}
