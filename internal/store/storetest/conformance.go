// Package storetest is the behavioural conformance suite every store.Store
// implementation must pass. It is driven from each implementation's own test
// package (sqlite, postgres) through Run, so the three stores are held to one
// contract instead of three drifting test files.
package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// Opener returns a fresh, migrated, empty store for one sub-test. The
// implementation is responsible for cleanup via t.Cleanup.
type Opener func(t *testing.T) store.Store

const tenant = "test-tenant"

// Run executes every conformance sub-test against stores produced by open.
func Run(t *testing.T, open Opener) {
	t.Helper()
	suites := []struct {
		name string
		fn   func(t *testing.T, s store.Store)
	}{
		{"Devices", testDevices},
		{"Messages", testMessages},
		{"MessageDuplicateIsNoOp", testMessageDuplicate},
		{"Webhooks", testWebhooks},
		{"Positions", testPositions},
		{"AuditLog", testAuditLog},
		{"TenantIsolation", testTenantIsolation},
		{"APIKeys", testAPIKeys},
		{"APIKeyTenantIsolation", testAPIKeyTenantIsolation},
		{"DeviceConfigVersioning", testDeviceConfigVersioning},
		{"SystemConfig", testSystemConfig},
		{"ClaimsAndDeadman", testClaims},
		{"TenantsAndInvites", testTenants},
		{"OIDCIdentities", testOIDCIdentities},
		{"TenantOffboarding", testTenantOffboarding},
		{"TenantQuota", testTenantQuota},
		{"Receipts", testReceipts},
		{"ReceiptPaymentRef", testReceiptPaymentRef},
		{"OIDCClaim", testOIDCClaim},
		{"ReceiptLease", testReceiptLease},
		{"BuyerCountry", testBuyerCountry},
		{"Refunds", testRefunds},
		{"Stripe", testStripe},
		{"HostedTAK", testHostedTAK},
	}
	for _, s := range suites {
		t.Run(s.name, func(t *testing.T) {
			s.fn(t, open(t))
		})
	}
}

func testDevices(t *testing.T, db store.Store) {
	ctx := context.Background()
	d := &store.Device{IMEI: "300234063904190", Label: "Field Unit 1", Type: "rockblock", Notes: "n"}
	if err := db.CreateDevice(ctx, tenant, d); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := db.GetDevice(ctx, tenant, d.IMEI)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Label != "Field Unit 1" || got.Type != "rockblock" {
		t.Errorf("get: %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at should be set")
	}
	got.Label = "Renamed"
	if err := db.UpdateDevice(ctx, tenant, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := db.TouchDeviceLastSeen(ctx, tenant, d.IMEI); err != nil {
		t.Fatalf("touch: %v", err)
	}
	all, err := db.ListDevices(ctx, tenant)
	if err != nil || len(all) != 1 || all[0].Label != "Renamed" {
		t.Fatalf("list: %v %+v", err, all)
	}
	if all[0].LastSeen.IsZero() || time.Since(all[0].LastSeen) > time.Minute {
		t.Errorf("last_seen not touched: %v", all[0].LastSeen)
	}
	if err := db.DeleteDevice(ctx, tenant, d.IMEI); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.GetDevice(ctx, tenant, d.IMEI); err == nil {
		t.Error("get after delete should fail")
	}
}

func testMessages(t *testing.T, db store.Store) {
	ctx := context.Background()
	_ = db.CreateDevice(ctx, tenant, &store.Device{IMEI: "300234063904190", Label: "T", Type: "rockblock"})
	msg := &store.Message{DeviceIMEI: "300234063904190", Direction: "mo", Channel: "iridium", MOMSN: 42, Text: "Hello from field", RawHex: "48656c6c6f", Status: "received"}
	if err := db.InsertMessage(ctx, tenant, msg); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if msg.ID == "" {
		t.Error("ID should be generated when empty")
	}
	msgs, err := db.ListMessages(ctx, tenant, "300234063904190", 10)
	if err != nil || len(msgs) != 1 || msgs[0].Text != "Hello from field" {
		t.Fatalf("list: %v %+v", err, msgs)
	}
	got, err := db.GetMessage(ctx, tenant, msg.ID)
	if err != nil || got.MOMSN != 42 || got.CreatedAt.IsZero() {
		t.Fatalf("get: %v %+v", err, got)
	}
	if err := db.UpdateMessageStatus(ctx, tenant, msg.ID, "delivered", ""); err != nil {
		t.Fatalf("update status: %v", err)
	}
	got, _ = db.GetMessage(ctx, tenant, msg.ID)
	if got.Status != "delivered" {
		t.Errorf("status: %q", got.Status)
	}

	// Scheduled messages are returned by ListScheduledMessages once due.
	sched := &store.Message{DeviceIMEI: "300234063904190", Direction: "mt", Channel: "iridium", Text: "later", Status: "scheduled", ScheduledAt: time.Now().Add(-time.Minute)}
	if err := db.InsertMessage(ctx, tenant, sched); err != nil {
		t.Fatalf("insert scheduled: %v", err)
	}
	due, err := db.ListScheduledMessages(ctx, time.Now(), 10)
	if err != nil {
		t.Fatalf("list scheduled: %v", err)
	}
	found := false
	for _, m := range due {
		if m.ID == sched.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("due scheduled message not listed: %+v", due)
	}
}

func testMessageDuplicate(t *testing.T, db store.Store) {
	ctx := context.Background()
	_ = db.CreateDevice(ctx, tenant, &store.Device{IMEI: "300434060000001", Label: "dup", Type: "rockblock"})
	m := &store.Message{ID: "mo-300434060000001-7", DeviceIMEI: "300434060000001", Direction: "mo", Channel: "iridium", MOMSN: 7, Text: "hi", Status: "received"}
	if err := db.InsertMessage(ctx, tenant, m); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	again := *m
	again.Text = "changed"
	if err := db.InsertMessage(ctx, tenant, &again); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("second insert: got %v, want ErrDuplicate", err)
	}
	msgs, _ := db.ListMessages(ctx, tenant, "300434060000001", 10)
	if len(msgs) != 1 || msgs[0].Text != "hi" {
		t.Fatalf("expected exactly one unchanged row, got %+v", msgs)
	}
}

func testWebhooks(t *testing.T, db store.Store) {
	ctx := context.Background()
	wh := &store.WebhookConfig{ID: "wh-1", URL: "https://example.com/hook", Secret: "s3cret", Events: []string{"mo", "sos"}, Enabled: true}
	if err := db.SaveWebhook(ctx, tenant, wh); err != nil {
		t.Fatalf("save: %v", err)
	}
	wh.URL = "https://example.com/hook2"
	if err := db.SaveWebhook(ctx, tenant, wh); err != nil {
		t.Fatalf("save (upsert): %v", err)
	}
	all, err := db.ListWebhooks(ctx, tenant)
	if err != nil || len(all) != 1 {
		t.Fatalf("list: %v %+v", err, all)
	}
	if all[0].URL != "https://example.com/hook2" || len(all[0].Events) != 2 || !all[0].Enabled {
		t.Errorf("upsert result: %+v", all[0])
	}
	if err := db.InsertDeliveryLog(ctx, tenant, &store.DeliveryLog{WebhookID: "wh-1", Event: "mo", DeviceIMEI: "x", StatusCode: 200, Attempt: 1}); err != nil {
		t.Fatalf("delivery log: %v", err)
	}
	logs, err := db.ListDeliveryLogs(ctx, tenant, 10)
	if err != nil || len(logs) != 1 || logs[0].StatusCode != 200 {
		t.Fatalf("delivery logs: %v %+v", err, logs)
	}
	if err := db.DeleteWebhook(ctx, tenant, "wh-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if all2, _ := db.ListWebhooks(ctx, tenant); len(all2) != 0 {
		t.Errorf("after delete: %d", len(all2))
	}
}

func testPositions(t *testing.T, db store.Store) {
	ctx := context.Background()
	_ = db.CreateDevice(ctx, tenant, &store.Device{IMEI: "300234063904190", Label: "Test", Type: "rockblock"})
	p1 := &store.Position{DeviceIMEI: "300234063904190", Lat: 52.3676, Lon: 4.9041, Source: "iridium_cep", CEP: 10.0}
	if err := db.InsertPosition(ctx, tenant, p1); err != nil {
		t.Fatalf("insert p1: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // ordering by created_at
	p2 := &store.Position{DeviceIMEI: "300234063904190", Lat: 52.37, Lon: 4.91, Source: "gps"}
	if err := db.InsertPosition(ctx, tenant, p2); err != nil {
		t.Fatalf("insert p2: %v", err)
	}
	latest, err := db.LatestPosition(ctx, tenant, "300234063904190")
	if err != nil || latest.Source != "gps" {
		t.Fatalf("latest: %v %+v", err, latest)
	}
	all, _ := db.ListPositions(ctx, tenant, "300234063904190", 10)
	if len(all) != 2 {
		t.Errorf("list: %d", len(all))
	}
	ranged, total, err := db.ListPositionsRange(ctx, tenant, "300234063904190", time.Now().Add(-time.Hour), time.Now().Add(time.Hour), 1, 0)
	if err != nil || total != 2 || len(ranged) != 1 {
		t.Errorf("range: err=%v total=%d n=%d", err, total, len(ranged))
	}
}

func testAuditLog(t *testing.T, db store.Store) {
	ctx := context.Background()
	if err := db.InsertAuditEntry(ctx, tenant, &store.AuditEntry{Action: "login", Actor: "admin", IP: "1.2.3.4", Hash: "h1"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := db.InsertAuditEntry(ctx, tenant, &store.AuditEntry{Action: "device.create", Actor: "admin", Detail: "IMEI=1", PrevHash: "h1", Hash: "h2"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	entries, err := db.ListAuditEntries(ctx, tenant, 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("list: %v %d", err, len(entries))
	}
	// limit 0 = the whole chain (audit.VerifyChain relies on it); limit 1 = newest only.
	if all, err := db.ListAuditEntries(ctx, tenant, 0); err != nil || len(all) != 2 {
		t.Fatalf("list limit 0 must return every entry: %v %d", err, len(all))
	}
	if one, err := db.ListAuditEntries(ctx, tenant, 1); err != nil || len(one) != 1 || one[0].Hash != "h2" {
		t.Fatalf("list limit 1: %v %+v", err, one)
	}
	latest, err := db.GetLatestAuditEntry(ctx, tenant)
	if err != nil || latest.Hash != "h2" {
		t.Fatalf("latest: %v %+v", err, latest)
	}
	old, err := db.ListAuditEntriesBefore(ctx, tenant, time.Now().Add(time.Hour), 10)
	if err != nil || len(old) != 2 {
		t.Fatalf("before: %v %d", err, len(old))
	}
	n, err := db.DeleteAuditEntriesBefore(ctx, tenant, time.Now().Add(time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("delete before: %v %d", err, n)
	}
}

func testTenantIsolation(t *testing.T, db store.Store) {
	ctx := context.Background()
	a, b := "tenant-alpha", "tenant-beta"
	if err := db.CreateDevice(ctx, a, &store.Device{IMEI: "111111111111111", Label: "Alpha Device", Type: "rockblock"}); err != nil {
		t.Fatalf("create A: %v", err)
	}
	if err := db.CreateDevice(ctx, b, &store.Device{IMEI: "222222222222222", Label: "Beta Device", Type: "globalstar"}); err != nil {
		t.Fatalf("create B: %v", err)
	}
	if devs, _ := db.ListDevices(ctx, a); len(devs) != 1 || devs[0].Label != "Alpha Device" {
		t.Fatalf("tenant A devices: %+v", devs)
	}
	if _, err := db.GetDevice(ctx, a, "222222222222222"); err == nil {
		t.Error("tenant A must not read tenant B's device")
	}
	_ = db.DeleteDevice(ctx, b, "111111111111111")
	if _, err := db.GetDevice(ctx, a, "111111111111111"); err != nil {
		t.Error("tenant B must not delete tenant A's device")
	}
	_ = db.InsertMessage(ctx, a, &store.Message{DeviceIMEI: "111111111111111", Direction: "mo", Text: "alpha msg", Status: "received"})
	_ = db.InsertMessage(ctx, b, &store.Message{DeviceIMEI: "222222222222222", Direction: "mo", Text: "beta msg", Status: "received"})
	if msgs, _ := db.ListMessages(ctx, a, "", 100); len(msgs) != 1 || msgs[0].Text != "alpha msg" {
		t.Errorf("tenant A messages: %+v", msgs)
	}
	_ = db.InsertPosition(ctx, a, &store.Position{DeviceIMEI: "111111111111111", Lat: 1, Lon: 2, Source: "gps"})
	_ = db.InsertPosition(ctx, b, &store.Position{DeviceIMEI: "222222222222222", Lat: 3, Lon: 4, Source: "gps"})
	if pos, err := db.LatestPosition(ctx, a, "111111111111111"); err != nil || pos.Lat != 1 {
		t.Error("tenant A position mismatch")
	}
	if _, err := db.LatestPosition(ctx, a, "222222222222222"); err == nil {
		t.Error("tenant A must not see tenant B's position")
	}
	_ = db.InsertAuditEntry(ctx, a, &store.AuditEntry{Action: "login", Actor: "admin-a"})
	_ = db.InsertAuditEntry(ctx, b, &store.AuditEntry{Action: "login", Actor: "admin-b"})
	if audit, _ := db.ListAuditEntries(ctx, a, 100); len(audit) != 1 || audit[0].Actor != "admin-a" {
		t.Errorf("tenant A audit: %+v", audit)
	}
	// Tenant lookups (used by the routing engine and subscribers).
	if tid, err := db.LookupDeviceTenant(ctx, "111111111111111"); err != nil || tid != a {
		t.Errorf("LookupDeviceTenant: %v %q (want %q)", err, tid, a)
	}
	if _, err := db.LookupDeviceTenant(ctx, "000000000000000"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LookupDeviceTenant unknown: %v", err)
	}
	if _, err := db.LookupBridgeTenant(ctx, "no-such-bridge"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LookupBridgeTenant unknown: %v", err)
	}
}

func testAPIKeys(t *testing.T, db store.Store) {
	ctx := context.Background()
	key := &store.APIKey{KeyHash: "sha256_abc123", KeyPrefix: "meshsat_ab12cd34", Role: "operator", Label: "CI pipeline key", DeviceIMEI: "300234063904190", ExpiresAt: time.Now().Add(24 * time.Hour)}
	if err := db.CreateAPIKey(ctx, tenant, key); err != nil {
		t.Fatalf("create: %v", err)
	}
	if key.ID == "" {
		t.Error("ID should be auto-generated")
	}
	got, tid, err := db.GetAPIKeyByHash(ctx, "sha256_abc123")
	if err != nil || tid != tenant || got.Role != "operator" || got.Label != "CI pipeline key" || got.ExpiresAt.IsZero() {
		t.Fatalf("get by hash: %v %q %+v", err, tid, got)
	}
	byID, err := db.GetAPIKeyByID(ctx, tenant, key.ID)
	if err != nil || byID.KeyPrefix != "meshsat_ab12cd34" {
		t.Fatalf("get by id: %v %+v", err, byID)
	}
	if keys, _ := db.ListAPIKeys(ctx, tenant); len(keys) != 1 {
		t.Fatalf("list: %d", len(keys))
	}
	if err := db.TouchAPIKeyLastUsed(ctx, key.ID); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if updated, _, _ := db.GetAPIKeyByHash(ctx, "sha256_abc123"); updated.LastUsed.IsZero() {
		t.Error("last_used should be set after touch")
	}
	expiring, err := db.ListExpiringAPIKeys(ctx, time.Now().Add(48*time.Hour), 10)
	if err != nil || len(expiring) != 1 {
		t.Errorf("expiring: %v %d", err, len(expiring))
	}
	if err := db.UpdateAPIKeySecret(ctx, tenant, key.ID, "sha256_new", "meshsat_newprefx", time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, _, err := db.GetAPIKeyByHash(ctx, "sha256_new"); err != nil {
		t.Errorf("rotated hash not found: %v", err)
	}
	if err := db.DeleteAPIKey(ctx, tenant, key.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if keys, _ := db.ListAPIKeys(ctx, tenant); len(keys) != 0 {
		t.Errorf("after delete: %d", len(keys))
	}
}

func testAPIKeyTenantIsolation(t *testing.T, db store.Store) {
	ctx := context.Background()
	keyA := &store.APIKey{KeyHash: "hash_a", KeyPrefix: "meshsat_aaaaaaaa", Role: "owner", Label: "Key A"}
	keyB := &store.APIKey{KeyHash: "hash_b", KeyPrefix: "meshsat_bbbbbbbb", Role: "viewer", Label: "Key B"}
	_ = db.CreateAPIKey(ctx, "tenant-a", keyA)
	_ = db.CreateAPIKey(ctx, "tenant-b", keyB)
	if keysA, _ := db.ListAPIKeys(ctx, "tenant-a"); len(keysA) != 1 || keysA[0].Label != "Key A" {
		t.Errorf("tenant-a keys: %+v", keysA)
	}
	_ = db.DeleteAPIKey(ctx, "tenant-b", keyA.ID)
	if keysA, _ := db.ListAPIKeys(ctx, "tenant-a"); len(keysA) != 1 {
		t.Error("tenant-b must not delete tenant-a's key")
	}
	if _, tid, err := db.GetAPIKeyByHash(ctx, "hash_a"); err != nil || tid != "tenant-a" {
		t.Errorf("get: %v %q", err, tid)
	}
}

func testDeviceConfigVersioning(t *testing.T, db store.Store) {
	ctx := context.Background()
	_ = db.CreateDevice(ctx, tenant, &store.Device{IMEI: "300234063904190", Label: "Test"})
	c1 := &store.DeviceConfig{DeviceIMEI: "300234063904190", Config: `{"reporting_interval":60}`, Author: "user-1", Comment: "Initial"}
	if err := db.CreateDeviceConfig(ctx, tenant, c1); err != nil {
		t.Fatalf("create v1: %v", err)
	}
	if c1.Version != 1 {
		t.Errorf("version: %d", c1.Version)
	}
	c2 := &store.DeviceConfig{DeviceIMEI: "300234063904190", Config: `{"reporting_interval":30}`, Author: "user-1", Comment: "Faster"}
	if err := db.CreateDeviceConfig(ctx, tenant, c2); err != nil {
		t.Fatalf("create v2: %v", err)
	}
	if c2.Version != 2 {
		t.Errorf("version: %d", c2.Version)
	}
	latest, err := db.GetDeviceConfigLatest(ctx, tenant, "300234063904190")
	if err != nil || latest.Version != 2 {
		t.Fatalf("latest: %v %+v", err, latest)
	}
	v1, err := db.GetDeviceConfigVersion(ctx, tenant, "300234063904190", 1)
	if err != nil || v1.Comment != "Initial" {
		t.Fatalf("v1: %v %+v", err, v1)
	}
	versions, err := db.ListDeviceConfigVersions(ctx, tenant, "300234063904190", 10)
	if err != nil || len(versions) != 2 {
		t.Fatalf("versions: %v %d", err, len(versions))
	}
	if _, err := db.GetDeviceConfigLatest(ctx, "other-tenant", "300234063904190"); err == nil {
		t.Error("other tenant must not read the config")
	}
}

func testSystemConfig(t *testing.T, db store.Store) {
	ctx := context.Background()
	if err := db.SetSystemConfig(ctx, "bridge_ca_cert", "-----BEGIN-----"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := db.SetSystemConfig(ctx, "bridge_ca_cert", "-----BEGIN v2-----"); err != nil {
		t.Fatalf("set (upsert): %v", err)
	}
	v, err := db.GetSystemConfig(ctx, "bridge_ca_cert")
	if err != nil || v != "-----BEGIN v2-----" {
		t.Fatalf("get: %v %q", err, v)
	}
	if _, err := db.GetSystemConfig(ctx, "missing-key"); err == nil {
		t.Error("missing key should error")
	}
}

func testClaims(t *testing.T, db store.Store) {
	ctx := context.Background()
	won, err := db.ClaimOnce(ctx, "route:t:m1:r1")
	if err != nil || !won {
		t.Fatalf("first claim: won=%v err=%v", won, err)
	}
	won, err = db.ClaimOnce(ctx, "route:t:m1:r1")
	if err != nil || won {
		t.Fatalf("second claim must lose: won=%v err=%v", won, err)
	}
	if won, _ = db.ClaimOnce(ctx, "route:t:m1:r2"); !won {
		t.Error("a different key is a different claim")
	}
	if n, err := db.PurgeClaims(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Errorf("purge of fresh claims: n=%d err=%v", n, err)
	}
	if n, err := db.PurgeClaims(ctx, time.Now().Add(time.Hour)); err != nil || n != 2 {
		t.Errorf("purge all: n=%d err=%v", n, err)
	}
	if won, _ = db.ClaimOnce(ctx, "route:t:m1:r1"); !won {
		t.Error("claim can be re-taken after purge")
	}

	// Scheduled message claim: scheduled -> sending exactly once.
	_ = db.CreateDevice(ctx, tenant, &store.Device{IMEI: "300234063904190", Label: "T", Type: "rockblock"})
	m := &store.Message{DeviceIMEI: "300234063904190", Direction: "mt", Channel: "iridium", Text: "later", Status: "scheduled", ScheduledAt: time.Now().Add(-time.Minute)}
	if err := db.InsertMessage(ctx, tenant, m); err != nil {
		t.Fatal(err)
	}
	if won, err := db.ClaimScheduledMessage(ctx, m.ID); err != nil || !won {
		t.Fatalf("first scheduled claim: %v %v", won, err)
	}
	if won, err := db.ClaimScheduledMessage(ctx, m.ID); err != nil || won {
		t.Fatalf("second scheduled claim must lose: %v %v", won, err)
	}
	got, _ := db.GetMessage(ctx, tenant, m.ID)
	if got.Status != "sending" {
		t.Errorf("status after claim: %q", got.Status)
	}
	if due, _ := db.ListScheduledMessages(ctx, time.Now(), 10); len(due) != 0 {
		t.Errorf("claimed message must not be listed as due: %+v", due)
	}
	if n, err := db.ExpireStaleSends(ctx, time.Hour); err != nil || n != 0 {
		t.Errorf("fresh send must not expire: n=%d err=%v", n, err)
	}
	if n, err := db.ExpireStaleSends(ctx, -time.Minute); err != nil || n != 1 {
		t.Errorf("stale send must expire: n=%d err=%v", n, err)
	}
	got, _ = db.GetMessage(ctx, tenant, m.ID)
	if got.Status != "failed" {
		t.Errorf("status after expiry: %q", got.Status)
	}

	// Alert compare-and-set.
	base := time.Now().UTC().Truncate(time.Second)
	a := &store.Alert{ID: "alert-cas", ChainID: "c", DeviceIMEI: "300234063904190", Type: "sos", State: store.AlertStateTriggered, NextEscAt: base, CreatedAt: base, UpdatedAt: base}
	if err := db.CreateAlert(ctx, tenant, a); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetAlert(ctx, tenant, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	lease := *stored
	lease.State = store.AlertStateEscalating
	lease.NextEscAt = base.Add(2 * time.Minute)
	lease.UpdatedAt = base.Add(time.Second)
	if won, err := db.AdvanceAlert(ctx, tenant, &lease, stored.NextEscAt); err != nil || !won {
		t.Fatalf("CAS with the current next_esc_at must win: %v %v", won, err)
	}
	again := *stored
	again.NextEscAt = base.Add(5 * time.Minute)
	if won, err := db.AdvanceAlert(ctx, tenant, &again, stored.NextEscAt); err != nil || won {
		t.Fatalf("CAS with a stale next_esc_at must lose: %v %v", won, err)
	}
	if won, _ := db.AdvanceAlert(ctx, "other-tenant", &lease, lease.NextEscAt); won {
		t.Error("CAS must be tenant scoped")
	}
	after, _ := db.GetAlert(ctx, tenant, a.ID)
	if after.State != store.AlertStateEscalating || !after.NextEscAt.Equal(lease.NextEscAt) {
		t.Errorf("CAS result not applied: %+v", after)
	}

	// Dead man's switch configs.
	dc := &store.DeadmanConfig{DeviceIMEI: "300234063904190", ChainID: "c", IntervalSec: 3600, GraceSec: 600, Enabled: true}
	if err := db.SaveDeadmanConfig(ctx, tenant, dc); err != nil {
		t.Fatal(err)
	}
	dc.SnoozedUntil = base.Add(time.Hour)
	dc.Alerted = true
	if err := db.SaveDeadmanConfig(ctx, tenant, dc); err != nil {
		t.Fatal(err)
	}
	rd, err := db.GetDeadmanConfig(ctx, tenant, dc.DeviceIMEI)
	if err != nil || !rd.Alerted || rd.SnoozedUntil.IsZero() || rd.IntervalSec != 3600 || rd.TenantID != tenant {
		t.Fatalf("deadman round trip: %v %+v", err, rd)
	}
	if list, _ := db.ListDeadmanConfigs(ctx); len(list) != 1 {
		t.Errorf("list deadman: %d", len(list))
	}
	if _, err := db.GetDeadmanConfig(ctx, "other-tenant", dc.DeviceIMEI); err == nil {
		t.Error("deadman config must be tenant scoped")
	}
	if err := db.DeleteDeadmanConfig(ctx, tenant, dc.DeviceIMEI); err != nil {
		t.Fatal(err)
	}
	if list, _ := db.ListDeadmanConfigs(ctx); len(list) != 0 {
		t.Errorf("list after delete: %d", len(list))
	}
}

func testTenants(t *testing.T, db store.Store) {
	ctx := context.Background()
	def, err := db.GetTenant(ctx, store.DefaultTenantID)
	if err != nil || def.Slug != "default" {
		t.Fatalf("default tenant must be seeded by migrations: %v %+v", err, def)
	}
	if err := db.CreateTenant(ctx, &store.Tenant{ID: "bridge", Name: "x"}); !errors.Is(err, store.ErrReservedTenantID) {
		t.Errorf("reserved id must be rejected, got %v", err)
	}
	tn := &store.Tenant{Name: "Alpine SAR", Slug: "alpine-sar"}
	if err := db.CreateTenant(ctx, tn); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A tenant created with no plan lands on the free tier, not on the
	// unlimited pre-tier "beta" (MESHSAT-989).
	if tn.ID == "" || tn.Plan != "free" || tn.Status != "active" || tn.CreatedAt.IsZero() {
		t.Errorf("defaults not applied: %+v", tn)
	}
	if got, err := db.GetTenantBySlug(ctx, "alpine-sar"); err != nil || got.ID != tn.ID {
		t.Errorf("by slug: %v %+v", err, got)
	}
	// Subscription columns round-trip (MESHSAT-989). kofi_payer_email is what
	// matches a Ko-fi renewal, which carries no message and so no claim code;
	// a store that dropped it on read would lapse a paying subscriber.
	tn.KofiClaimCode, tn.KofiPayerEmail = "AB2K9XYZ", "jo.example@example.com"
	expires := time.Now().UTC().Add(32 * 24 * time.Hour).Truncate(time.Second)
	tn.PlanExpiresAt = &expires
	if err := db.UpdateTenant(ctx, tn); err != nil {
		t.Fatalf("update subscription fields: %v", err)
	}
	if got, err := db.GetTenant(ctx, tn.ID); err != nil {
		t.Fatalf("reread: %v", err)
	} else if got.KofiClaimCode != "AB2K9XYZ" || got.KofiPayerEmail != "jo.example@example.com" ||
		got.PlanExpiresAt == nil || !got.PlanExpiresAt.Equal(expires) {
		t.Errorf("subscription fields did not round-trip: claim=%q payer=%q expires=%v",
			got.KofiClaimCode, got.KofiPayerEmail, got.PlanExpiresAt)
	}

	tn.Name, tn.OwnerUserID = "Alpine SAR e.V.", "usr-1"
	if err := db.UpdateTenant(ctx, tn); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetTenant(ctx, tn.ID)
	if got.Name != "Alpine SAR e.V." || got.OwnerUserID != "usr-1" {
		t.Errorf("update not applied: %+v", got)
	}
	if all, _ := db.ListTenants(ctx); len(all) != 2 {
		t.Errorf("list: %d", len(all))
	}
	if err := db.CreateTenant(ctx, &store.Tenant{Name: "dup", Slug: "alpine-sar"}); err == nil {
		t.Error("duplicate slug must fail")
	}

	// Invites.
	inv := &store.TenantInvite{Email: "  Ops@Example.ORG ", Role: "operator", TokenHash: "h"}
	if err := db.CreateInvite(ctx, tn.ID, inv); err != nil {
		t.Fatal(err)
	}
	if inv.Email != "ops@example.org" || inv.ExpiresAt.IsZero() || inv.ID == "" {
		t.Errorf("invite normalisation: %+v", inv)
	}
	p, err := db.GetPendingInviteByEmail(ctx, "OPS@example.org")
	if err != nil || p.ID != inv.ID || p.TenantID != tn.ID || p.Role != "operator" {
		t.Fatalf("pending by email: %v %+v", err, p)
	}
	if list, _ := db.ListInvites(ctx, tn.ID); len(list) != 1 {
		t.Errorf("list invites: %d", len(list))
	}
	if err := db.AcceptInvite(ctx, inv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetPendingInviteByEmail(ctx, "ops@example.org"); err == nil {
		t.Error("accepted invite must not be pending")
	}
	exp := &store.TenantInvite{Email: "late@example.org", ExpiresAt: time.Now().Add(-time.Hour)}
	_ = db.CreateInvite(ctx, tn.ID, exp)
	if _, err := db.GetPendingInviteByEmail(ctx, "late@example.org"); err == nil {
		t.Error("expired invite must not be pending")
	}
	if err := db.DeleteInvite(ctx, "other", inv.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := db.ListInvites(ctx, tn.ID); len(list) != 2 {
		t.Errorf("delete must be tenant scoped: %d", len(list))
	}
	if err := db.DeleteInvite(ctx, tn.ID, inv.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := db.ListInvites(ctx, tn.ID); len(list) != 1 {
		t.Errorf("after delete: %d", len(list))
	}
}

func testOIDCIdentities(t *testing.T, db store.Store) {
	ctx := context.Background()
	if _, err := db.GetOIDCIdentity(ctx, "https://idp/", "sub-1"); err == nil {
		t.Error("missing identity must error")
	}
	id := &store.OIDCIdentity{Issuer: "https://idp/", Subject: "sub-1", UserID: "usr-1", TenantID: tenant, Email: "a@example.org"}
	if err := db.LinkOIDCIdentity(ctx, id); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetOIDCIdentity(ctx, "https://idp/", "sub-1")
	if err != nil || got.UserID != "usr-1" || got.TenantID != tenant || got.PlatformAdmin || got.CreatedAt.IsZero() {
		t.Fatalf("round trip: %v %+v", err, got)
	}
	id.PlatformAdmin = true
	if err := db.LinkOIDCIdentity(ctx, id); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetOIDCIdentity(ctx, "https://idp/", "sub-1")
	if !got.PlatformAdmin || got.LastLoginAt.IsZero() {
		t.Errorf("upsert must update admin flag and last login: %+v", got)
	}
}
