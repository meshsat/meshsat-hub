package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cubeos-app/meshsat-hub/internal/store"
)

func testDB(t *testing.T) *DB {
	t.Helper()
	db, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestDeviceCRUD(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// Create
	dev := &store.Device{IMEI: "300234063904190", Label: "Field Unit 1", Type: "rockblock"}
	if err := db.CreateDevice(ctx, dev); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Get
	got, err := db.GetDevice(ctx, "300234063904190")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Label != "Field Unit 1" {
		t.Errorf("label: got %q", got.Label)
	}

	// List
	all, err := db.ListDevices(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("list: got %d, want 1", len(all))
	}

	// Update
	dev.Label = "Updated"
	if err := db.UpdateDevice(ctx, dev); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, _ := db.GetDevice(ctx, "300234063904190")
	if got2.Label != "Updated" {
		t.Errorf("updated label: got %q", got2.Label)
	}

	// TouchLastSeen
	if err := db.TouchDeviceLastSeen(ctx, "300234063904190"); err != nil {
		t.Fatalf("touch: %v", err)
	}

	// Delete
	if err := db.DeleteDevice(ctx, "300234063904190"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	all2, _ := db.ListDevices(ctx)
	if len(all2) != 0 {
		t.Errorf("after delete: got %d", len(all2))
	}
}

func TestMessageCRUD(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	db.CreateDevice(ctx, &store.Device{IMEI: "300234063904190", Label: "Test", Type: "rockblock"})

	msg := &store.Message{
		DeviceIMEI: "300234063904190",
		Direction:  "mo",
		Channel:    "iridium",
		MOMSN:      42,
		Text:       "Hello from field",
		Status:     "received",
	}
	if err := db.InsertMessage(ctx, msg); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if msg.ID == "" {
		t.Error("expected auto-generated ID")
	}

	msgs, err := db.ListMessages(ctx, "300234063904190", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("list: got %d", len(msgs))
	}
	if msgs[0].Text != "Hello from field" {
		t.Errorf("text: got %q", msgs[0].Text)
	}

	got, err := db.GetMessage(ctx, msg.ID)
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
	if err := db.SaveWebhook(ctx, wh); err != nil {
		t.Fatalf("save: %v", err)
	}

	all, err := db.ListWebhooks(ctx)
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

	if err := db.DeleteWebhook(ctx, "wh-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	all2, _ := db.ListWebhooks(ctx)
	if len(all2) != 0 {
		t.Errorf("after delete: got %d", len(all2))
	}
}

func TestPositions(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	db.CreateDevice(ctx, &store.Device{IMEI: "300234063904190", Label: "Test", Type: "rockblock"})

	p1 := &store.Position{DeviceIMEI: "300234063904190", Lat: 52.3676, Lon: 4.9041, Source: "iridium_cep", CEP: 10.0}
	if err := db.InsertPosition(ctx, p1); err != nil {
		t.Fatalf("insert p1: %v", err)
	}
	// SQLite datetime('now') has 1s resolution — use rowid ordering via sequential IDs
	p2 := &store.Position{DeviceIMEI: "300234063904190", Lat: 52.3700, Lon: 4.9100, Source: "gps"}
	if err := db.InsertPosition(ctx, p2); err != nil {
		t.Fatalf("insert p2: %v", err)
	}

	latest, err := db.LatestPosition(ctx, "300234063904190")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest.Source != "gps" {
		t.Errorf("latest source: got %q, want gps", latest.Source)
	}

	all, _ := db.ListPositions(ctx, "300234063904190", 10)
	if len(all) != 2 {
		t.Errorf("list: got %d", len(all))
	}
}

func TestAuditLog(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	db.InsertAuditEntry(ctx, &store.AuditEntry{Action: "login", Actor: "admin", IP: "1.2.3.4"})
	db.InsertAuditEntry(ctx, &store.AuditEntry{Action: "device.create", Actor: "admin", Detail: "IMEI=300234063904190"})

	entries, err := db.ListAuditEntries(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("list: got %d", len(entries))
	}
}
