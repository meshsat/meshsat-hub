package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

const testTenant = "t-fleet"

func newBridge(id, label string) *store.Bridge {
	return &store.Bridge{
		BridgeID: id, Label: label, Hostname: id + ".local", Version: "1.7.0", Mode: "direct",
		LocationLat: 52.1, LocationLon: 4.3, LocationAlt: 12,
		Capabilities: `["iridium","lora"]`, ReticulumHash: "abcd", ReticulumPubkey: "pub",
		CoTType: "a-f-G-U-C-I", CoTCallsign: label, Online: true,
		LastBirth: `{"ts":1}`, LastHealth: `{"cpu":0.5}`,
	}
}

func TestBridgeLifecycle(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// Not found is sql.ErrNoRows, as in the MariaDB store.
	if _, err := db.GetBridge(ctx, testTenant, "nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetBridge missing: want ErrNoRows, got %v", err)
	}

	if err := db.CreateOrUpdateBridge(ctx, testTenant, newBridge("br-1", "Zulu")); err != nil {
		t.Fatalf("create: %v", err)
	}
	b, err := db.GetBridge(ctx, testTenant, "br-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !b.Online || b.LastSeen == nil || b.CertExpiry != nil || b.TenantID != testTenant {
		t.Fatalf("unexpected bridge after birth: %+v", b)
	}
	if b.LastSeen.Location() != time.UTC || b.CreatedAt.Location() != time.UTC {
		t.Fatalf("timestamps must be UTC: %v %v", b.LastSeen.Location(), b.CreatedAt.Location())
	}
	for name, got := range map[string]string{"capabilities": b.Capabilities, "last_birth": b.LastBirth, "last_health": b.LastHealth} {
		if got == "" || !bytes.Contains([]byte(got), []byte(`"`)) {
			t.Fatalf("%s did not round-trip through JSONB: %q", name, got)
		}
	}

	// Empty JSON strings become the JSONB defaults instead of failing.
	empty := newBridge("br-empty", "Empty")
	empty.Capabilities, empty.LastBirth, empty.LastHealth = "", "", ""
	if err := db.CreateOrUpdateBridge(ctx, testTenant, empty); err != nil {
		t.Fatalf("create with empty JSON: %v", err)
	}
	if e, err := db.GetBridge(ctx, testTenant, "br-empty"); err != nil || e.Capabilities != "[]" || e.LastBirth != "{}" {
		t.Fatalf("empty JSON defaults: %+v %v", e, err)
	}

	// Credentials and certificate.
	if err := db.SetBridgeCredentials(ctx, testTenant, "br-1", "mqtt-br-1", "$2a$hash"); err != nil {
		t.Fatalf("set creds: %v", err)
	}
	expiry := time.Now().Add(90 * 24 * time.Hour).Truncate(time.Second)
	if err := db.SetBridgeCertificate(ctx, testTenant, "br-1", "-----BEGIN CERTIFICATE-----", expiry); err != nil {
		t.Fatalf("set cert: %v", err)
	}
	creds, err := db.GetBridgeCredentials(ctx, testTenant, "br-1")
	if err != nil {
		t.Fatalf("get creds: %v", err)
	}
	if creds.Username != "mqtt-br-1" || creds.Password != "$2a$hash" || creds.CertPEM == "" ||
		creds.CertExpiry == nil || !creds.CertExpiry.Equal(expiry) {
		t.Fatalf("credentials round trip: %+v", creds)
	}

	// A second birth must not clobber credentials or the certificate.
	rebirth := newBridge("br-1", "Zulu v2")
	rebirth.Version = "1.8.0"
	rebirth.LastBirth = `{"ts":2}`
	if err := db.CreateOrUpdateBridge(ctx, testTenant, rebirth); err != nil {
		t.Fatalf("rebirth: %v", err)
	}
	after, err := db.GetBridgeCredentials(ctx, testTenant, "br-1")
	if err != nil {
		t.Fatalf("get creds after rebirth: %v", err)
	}
	if after.Username != "mqtt-br-1" || after.Password != "$2a$hash" || after.CertPEM == "" || after.CertExpiry == nil {
		t.Fatalf("birth upsert clobbered credentials: %+v", after)
	}
	b, _ = db.GetBridge(ctx, testTenant, "br-1")
	if b.Version != "1.8.0" || b.Label != "Zulu v2" || !bytes.Contains([]byte(b.LastBirth), []byte("2")) {
		t.Fatalf("birth upsert did not update birth fields: %+v", b)
	}
	if b.MQTTPasswordHash != "" {
		t.Fatal("GetBridge must not expose the password hash")
	}

	// ListBridgesWithCredentials exposes the hash and skips bridges without a username.
	withCreds, err := db.ListBridgesWithCredentials(ctx)
	if err != nil {
		t.Fatalf("list with creds: %v", err)
	}
	if len(withCreds) != 1 || withCreds[0].BridgeID != "br-1" || withCreds[0].MQTTPasswordHash != "$2a$hash" {
		t.Fatalf("ListBridgesWithCredentials: %+v", withCreds)
	}

	// ListBridges: ordered by label, tenant scoped.
	if err := db.CreateOrUpdateBridge(ctx, testTenant, newBridge("br-2", "Alpha")); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateOrUpdateBridge(ctx, "other-tenant", newBridge("br-x", "Aaa")); err != nil {
		t.Fatal(err)
	}
	list, err := db.ListBridges(ctx, testTenant)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 || list[0].BridgeID != "br-2" || list[1].BridgeID != "br-empty" || list[2].BridgeID != "br-1" {
		t.Fatalf("ListBridges order/scope: %v", ids(list))
	}
	if _, err := db.GetBridge(ctx, "other-tenant", "br-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant GetBridge must be not found, got %v", err)
	}

	// Partial update, health, online.
	label, callsign := "Renamed", "CS-1"
	if err := db.UpdateBridge(ctx, testTenant, "br-1", store.BridgeUpdate{Label: &label, CoTCallsign: &callsign}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := db.UpdateBridge(ctx, testTenant, "br-1", store.BridgeUpdate{}); err != nil {
		t.Fatalf("empty update: %v", err)
	}
	if err := db.SetBridgeHealth(ctx, testTenant, "br-1", `{"cpu":0.9}`); err != nil {
		t.Fatalf("health: %v", err)
	}
	if err := db.SetBridgeOnline(ctx, testTenant, "br-1", false); err != nil {
		t.Fatalf("online: %v", err)
	}
	b, _ = db.GetBridge(ctx, testTenant, "br-1")
	if b.Label != "Renamed" || b.CoTCallsign != "CS-1" || b.Online || !bytes.Contains([]byte(b.LastHealth), []byte("0.9")) {
		t.Fatalf("after updates: %+v", b)
	}

	// Device association + delete clears devices.bridge_id.
	if _, err := db.rawDB.ExecContext(ctx, "INSERT INTO devices (imei, label, tenant_id) VALUES ('300001', 'dev', $1)", testTenant); err != nil {
		t.Fatal(err)
	}
	if err := db.AssociateDeviceWithBridge(ctx, testTenant, "300001", "br-1"); err != nil {
		t.Fatalf("associate: %v", err)
	}
	var bridgeID sql.NullString
	if err := db.rawDB.QueryRowContext(ctx, "SELECT bridge_id FROM devices WHERE imei='300001'").Scan(&bridgeID); err != nil || bridgeID.String != "br-1" {
		t.Fatalf("association not stored: %v %v", bridgeID, err)
	}
	if err := db.DeleteBridge(ctx, testTenant, "br-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := db.rawDB.QueryRowContext(ctx, "SELECT bridge_id FROM devices WHERE imei='300001'").Scan(&bridgeID); err != nil || bridgeID.Valid {
		t.Fatalf("delete must clear devices.bridge_id: %v %v", bridgeID, err)
	}
	if _, err := db.GetBridge(ctx, testTenant, "br-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted bridge still readable: %v", err)
	}
}

func ids(bs []*store.Bridge) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.BridgeID
	}
	return out
}

func TestMarkStaleBridgesOffline(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	for _, id := range []string{"stale", "fresh", "never-seen"} {
		if err := db.CreateOrUpdateBridge(ctx, testTenant, newBridge(id, id)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.rawDB.ExecContext(ctx, "UPDATE bridges SET last_seen = now() - interval '10 minutes' WHERE bridge_id='stale'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.rawDB.ExecContext(ctx, "UPDATE bridges SET last_seen = NULL WHERE bridge_id='never-seen'"); err != nil {
		t.Fatal(err)
	}
	if err := db.TouchBridgeLastSeen(ctx, testTenant, "fresh"); err != nil {
		t.Fatal(err)
	}

	n, err := db.MarkStaleBridgesOffline(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("mark stale: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows affected = %d, want 1", n)
	}
	for id, wantOnline := range map[string]bool{"stale": false, "fresh": true, "never-seen": true} {
		b, err := db.GetBridge(ctx, testTenant, id)
		if err != nil {
			t.Fatal(err)
		}
		if b.Online != wantOnline {
			t.Errorf("%s online = %v, want %v", id, b.Online, wantOnline)
		}
	}
	// Second pass is a no-op.
	if n, err := db.MarkStaleBridgesOffline(ctx, 5*time.Minute); err != nil || n != 0 {
		t.Fatalf("second pass: n=%d err=%v", n, err)
	}
}

func TestBondGroups(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if _, err := db.GetBondGroup(ctx, testTenant, "br-1", "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing bond group: want ErrNoRows, got %v", err)
	}
	g1 := &store.BondGroup{ID: "bg-1", Label: "Primary", Members: `["iridium","lora"]`, CostBudget: 1.5}
	g2 := &store.BondGroup{ID: "bg-2", Label: "Backup", Members: `["lora"]`, CostBudget: 0.25}
	if err := db.CreateBondGroup(ctx, testTenant, "br-1", g1); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.CreateBondGroup(ctx, testTenant, "br-1", g2); err != nil {
		t.Fatalf("create 2: %v", err)
	}
	// Same id on another bridge is allowed: PK is (tenant_id, bridge_id, id).
	if err := db.CreateBondGroup(ctx, testTenant, "br-2", &store.BondGroup{ID: "bg-1", Label: "Other"}); err != nil {
		t.Fatalf("create on second bridge: %v", err)
	}
	if err := db.CreateBondGroup(ctx, testTenant, "br-1", g1); err == nil {
		t.Fatal("duplicate PK must fail")
	}

	got, err := db.GetBondGroup(ctx, testTenant, "br-1", "bg-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Label != "Primary" || got.Members != g1.Members || got.CostBudget != 1.5 || got.BridgeID != "br-1" || got.TenantID != testTenant {
		t.Fatalf("round trip: %+v", got)
	}
	if _, err := time.Parse(time.RFC3339Nano, got.CreatedAt); err != nil {
		t.Fatalf("CreatedAt %q is not RFC3339Nano: %v", got.CreatedAt, err)
	}

	list, err := db.GetBondGroups(ctx, testTenant, "br-1")
	if err != nil || len(list) != 2 || list[0].ID != "bg-1" || list[1].ID != "bg-2" {
		t.Fatalf("list: %+v %v", list, err)
	}
	if other, _ := db.GetBondGroups(ctx, "other", "br-1"); len(other) != 0 {
		t.Fatalf("tenant scoping leaked: %+v", other)
	}

	g1.Label, g1.CostBudget = "Primary v2", 3
	if err := db.UpdateBondGroup(ctx, testTenant, "br-1", g1); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got, _ = db.GetBondGroup(ctx, testTenant, "br-1", "bg-1"); got.Label != "Primary v2" || got.CostBudget != 3 {
		t.Fatalf("update not applied: %+v", got)
	}
	if err := db.DeleteBondGroup(ctx, testTenant, "br-1", "bg-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if list, _ = db.GetBondGroups(ctx, testTenant, "br-1"); len(list) != 1 || list[0].ID != "bg-2" {
		t.Fatalf("after delete: %+v", list)
	}
	if got, err := db.GetBondGroup(ctx, testTenant, "br-2", "bg-1"); err != nil || got.Label != "Other" {
		t.Fatalf("other bridge's group must survive: %+v %v", got, err)
	}
}

func TestCostLedger(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	entries := []struct {
		id, imei, iface string
		cost            float64
		age             string
	}{
		{"c1", "300001", "iridium_sbd", 0.10, "0 hours"},
		{"c2", "300001", "iridium_sbd", 0.20, "1 hour"},
		{"c3", "300002", "globalstar", 0.05, "26 hours"},
		{"c4", "300002", "iridium_imt", 0.40, "72 hours"},
	}
	for _, e := range entries {
		if err := db.InsertCostEntry(ctx, testTenant, &store.CostEntry{
			ID: e.id, DeviceIMEI: e.imei, InterfaceType: e.iface, Direction: "mt", CostUSD: e.cost, MessageID: "m-" + e.id,
		}); err != nil {
			t.Fatalf("insert %s: %v", e.id, err)
		}
		if _, err := db.rawDB.ExecContext(ctx, "UPDATE cost_ledger SET created_at = now() - $1::interval WHERE id=$2", e.age, e.id); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.InsertCostEntry(ctx, "other", &store.CostEntry{ID: "cx", DeviceIMEI: "300001", CostUSD: 99}); err != nil {
		t.Fatal(err)
	}

	all, err := db.ListCostEntries(ctx, testTenant, "", time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 4 || all[0].ID != "c1" || all[3].ID != "c4" {
		t.Fatalf("list order (created_at DESC): %+v", all)
	}
	if all[0].CreatedAt.Location() != time.UTC {
		t.Fatalf("CreatedAt must be UTC")
	}
	byDev, _ := db.ListCostEntries(ctx, testTenant, "300002", time.Time{}, time.Time{}, 0)
	if len(byDev) != 2 {
		t.Fatalf("device filter: %+v", byDev)
	}
	limited, _ := db.ListCostEntries(ctx, testTenant, "", time.Time{}, time.Time{}, 1)
	if len(limited) != 1 {
		t.Fatalf("limit: %+v", limited)
	}
	recent, _ := db.ListCostEntries(ctx, testTenant, "", time.Now().Add(-2*time.Hour), time.Now(), 0)
	if len(recent) != 2 {
		t.Fatalf("time range: %+v", recent)
	}

	byDevice, err := db.AggregateCosts(ctx, testTenant, time.Time{}, time.Time{}, "device")
	if err != nil {
		t.Fatalf("aggregate device: %v", err)
	}
	if len(byDevice) != 2 || byDevice[0].GroupKey != "300002" || byDevice[0].Count != 2 || byDevice[1].GroupKey != "300001" {
		t.Fatalf("aggregate by device (ORDER BY total DESC): %+v", byDevice)
	}
	if got := byDevice[0].TotalUSD; got < 0.449 || got > 0.451 {
		t.Fatalf("total for 300002 = %v, want 0.45", got)
	}

	byDay, err := db.AggregateCosts(ctx, testTenant, time.Time{}, time.Time{}, "day")
	if err != nil {
		t.Fatalf("aggregate day: %v", err)
	}
	if len(byDay) < 2 {
		t.Fatalf("expected at least two day buckets, got %+v", byDay)
	}
	total := 0
	for _, a := range byDay {
		if _, err := time.Parse("2006-01-02", a.GroupKey); err != nil {
			t.Fatalf("day key %q is not YYYY-MM-DD", a.GroupKey)
		}
		total += a.Count
	}
	if total != 4 {
		t.Fatalf("day buckets should cover 4 rows, got %d", total)
	}
	byMonth, err := db.AggregateCosts(ctx, testTenant, time.Time{}, time.Time{}, "month")
	if err != nil || len(byMonth) == 0 {
		t.Fatalf("aggregate month: %+v %v", byMonth, err)
	}
	if _, err := time.Parse("2006-01", byMonth[0].GroupKey); err != nil {
		t.Fatalf("month key %q is not YYYY-MM", byMonth[0].GroupKey)
	}
	byIface, _ := db.AggregateCosts(ctx, testTenant, time.Time{}, time.Time{}, "interface")
	if len(byIface) != 3 || byIface[0].GroupKey != "iridium_imt" {
		t.Fatalf("aggregate interface: %+v", byIface)
	}
	windowed, _ := db.AggregateCosts(ctx, testTenant, time.Now().Add(-2*time.Hour), time.Now(), "device")
	if len(windowed) != 1 || windowed[0].Count != 2 {
		t.Fatalf("windowed aggregate: %+v", windowed)
	}
}

func TestDeviceGroups(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	for _, imei := range []string{"300001", "300002"} {
		if _, err := db.rawDB.ExecContext(ctx, "INSERT INTO devices (imei, label, tenant_id) VALUES ($1, $2, $3)", imei, "dev-"+imei, testTenant); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := db.GetDeviceGroup(ctx, testTenant, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing group: %v", err)
	}
	if err := db.CreateDeviceGroup(ctx, testTenant, &store.DeviceGroup{ID: "g-1", Name: "North", Description: "d", Color: "#ff0000"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.CreateDeviceGroup(ctx, testTenant, &store.DeviceGroup{ID: "g-2", Name: "East", Color: "#00ff00"}); err != nil {
		t.Fatalf("create 2: %v", err)
	}

	// Membership is idempotent.
	for i := 0; i < 3; i++ {
		if err := db.AddDeviceToGroup(ctx, testTenant, "g-1", "300001"); err != nil {
			t.Fatalf("add (attempt %d): %v", i, err)
		}
	}
	if err := db.AddDeviceToGroup(ctx, testTenant, "g-1", "300002"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddDeviceToGroup(ctx, testTenant, "g-2", "300002"); err != nil {
		t.Fatal(err)
	}
	g, err := db.GetDeviceGroup(ctx, testTenant, "g-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if g.MemberCount != 2 || g.Name != "North" || g.Color != "#ff0000" || g.CreatedAt.Location() != time.UTC {
		t.Fatalf("group: %+v", g)
	}

	groups, err := db.ListDeviceGroups(ctx, testTenant)
	if err != nil || len(groups) != 2 || groups[0].Name != "East" || groups[0].MemberCount != 1 || groups[1].MemberCount != 2 {
		t.Fatalf("list (ORDER BY name, member counts): %+v %v", groups, err)
	}
	devs, err := db.ListDevicesInGroup(ctx, testTenant, "g-1")
	if err != nil || len(devs) != 2 || devs[0].IMEI != "300001" || !devs[0].LastSeen.IsZero() {
		t.Fatalf("devices in group: %+v %v", devs, err)
	}
	forDev, err := db.ListGroupsForDevice(ctx, testTenant, "300002")
	if err != nil || len(forDev) != 2 || forDev[0].ID != "g-2" {
		t.Fatalf("groups for device: %+v %v", forDev, err)
	}

	if err := db.RemoveDeviceFromGroup(ctx, testTenant, "g-1", "300001"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if g, _ = db.GetDeviceGroup(ctx, testTenant, "g-1"); g.MemberCount != 1 {
		t.Fatalf("after remove: %+v", g)
	}
	if err := db.UpdateDeviceGroup(ctx, testTenant, &store.DeviceGroup{ID: "g-1", Name: "North2", Description: "x", Color: "#000000"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if g, _ = db.GetDeviceGroup(ctx, testTenant, "g-1"); g.Name != "North2" || g.Color != "#000000" {
		t.Fatalf("update not applied: %+v", g)
	}
	if err := db.DeleteDeviceGroup(ctx, testTenant, "g-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var n int
	if err := db.rawDB.QueryRowContext(ctx, "SELECT count(*) FROM device_group_members WHERE group_id='g-1'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("memberships must be deleted with the group: n=%d err=%v", n, err)
	}
	if _, err := db.GetDeviceGroup(ctx, testTenant, "g-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted group readable: %v", err)
	}
	if other, _ := db.ListDeviceGroups(ctx, "other"); len(other) != 0 {
		t.Fatalf("tenant leak: %+v", other)
	}
}

func TestMessageTemplates(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	tpl := &store.MessageTemplate{Name: "Check-in", Body: "Hi {{name}}", Variables: []string{"name"}}
	if err := db.CreateMessageTemplate(ctx, testTenant, tpl); err != nil {
		t.Fatalf("create: %v", err)
	}
	if tpl.ID == "" || tpl.ID[:5] != "tmpl-" || tpl.CreatedAt.IsZero() {
		t.Fatalf("id/timestamps not generated: %+v", tpl)
	}
	noVars := &store.MessageTemplate{ID: "tmpl-fixed", Name: "Alpha", Body: "static"}
	if err := db.CreateMessageTemplate(ctx, testTenant, noVars); err != nil {
		t.Fatalf("create fixed id: %v", err)
	}
	got, err := db.GetMessageTemplate(ctx, testTenant, tpl.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Check-in" || got.Body != "Hi {{name}}" || len(got.Variables) != 1 || got.Variables[0] != "name" {
		t.Fatalf("round trip: %+v", got)
	}
	if !got.CreatedAt.Equal(tpl.CreatedAt.Truncate(time.Microsecond)) {
		t.Fatalf("created_at drift: %v vs %v", got.CreatedAt, tpl.CreatedAt)
	}
	list, err := db.ListMessageTemplates(ctx, testTenant)
	if err != nil || len(list) != 2 || list[0].Name != "Alpha" || list[1].Name != "Check-in" || list[0].Variables != nil {
		t.Fatalf("list: %+v %v", list, err)
	}
	tpl.Name, tpl.Variables = "Check-in v2", []string{"name", "time"}
	if err := db.UpdateMessageTemplate(ctx, testTenant, tpl); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got, _ = db.GetMessageTemplate(ctx, testTenant, tpl.ID); got.Name != "Check-in v2" || len(got.Variables) != 2 || !got.UpdatedAt.After(got.CreatedAt) {
		t.Fatalf("update not applied: %+v", got)
	}
	if err := db.DeleteMessageTemplate(ctx, testTenant, tpl.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.GetMessageTemplate(ctx, testTenant, tpl.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted template readable: %v", err)
	}
	if _, err := db.GetMessageTemplate(ctx, "other", "tmpl-fixed"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant read: %v", err)
	}
}

func TestAlertRules(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	r := &store.AlertRule{Name: "Silent 6h", ConditionType: "device_not_seen", ConditionParams: `{"threshold_hours":6}`, ChainID: "chain-1", DeviceFilter: "*", Enabled: true}
	if err := db.CreateAlertRule(ctx, testTenant, r); err != nil {
		t.Fatalf("create: %v", err)
	}
	if r.ID == "" || r.ID[:6] != "arule-" {
		t.Fatalf("id not generated: %q", r.ID)
	}
	if err := db.CreateAlertRule(ctx, "other", &store.AlertRule{ID: "arule-other", Name: "Battery", ConditionType: "battery_low", Enabled: false}); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetAlertRule(ctx, testTenant, r.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != r.Name || got.ConditionParams != r.ConditionParams || !got.Enabled || !got.LastEvaluated.IsZero() || got.DeviceFilter != "*" {
		t.Fatalf("round trip: %+v", got)
	}
	if _, err := db.GetAlertRule(ctx, "other", r.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant get: %v", err)
	}

	scoped, err := db.ListAlertRules(ctx, testTenant)
	if err != nil || len(scoped) != 1 || scoped[0].TenantID != testTenant {
		t.Fatalf("scoped list: %+v %v", scoped, err)
	}
	all, err := db.ListAlertRules(ctx, "")
	if err != nil || len(all) != 2 || all[0].Name != "Battery" || all[0].TenantID != "other" || all[1].TenantID != testTenant {
		t.Fatalf("cross-tenant list (evaluator, ORDER BY name): %+v %v", all, err)
	}

	evalAt := time.Now().UTC().Truncate(time.Second)
	r.LastEvaluated, r.Enabled, r.Name = evalAt, false, "Silent 12h"
	if err := db.UpdateAlertRule(ctx, testTenant, r); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = db.GetAlertRule(ctx, testTenant, r.ID)
	if got.Name != "Silent 12h" || got.Enabled || !got.LastEvaluated.Equal(evalAt) || got.LastEvaluated.Location() != time.UTC {
		t.Fatalf("update not applied: %+v", got)
	}
	r.LastEvaluated = time.Time{}
	if err := db.UpdateAlertRule(ctx, testTenant, r); err != nil {
		t.Fatalf("update with zero last_evaluated: %v", err)
	}
	if got, _ = db.GetAlertRule(ctx, testTenant, r.ID); !got.LastEvaluated.IsZero() {
		t.Fatalf("zero LastEvaluated must read back as zero: %v", got.LastEvaluated)
	}
	if err := db.DeleteAlertRule(ctx, testTenant, r.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.GetAlertRule(ctx, testTenant, r.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted rule readable: %v", err)
	}
}

func TestCredentials(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	blob := []byte{0x00, 0x01, 0xff, 0xfe, 'g', 'c', 'm', 0x00, 0x7f}
	notAfter := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	c := &store.Credential{
		ID: "cred-1", Provider: "cloudloop_mqtt", Name: "Cloudloop mTLS", CredType: "mtls_bundle",
		EncryptedData: blob, CertNotAfter: &notAfter, CertSubject: "CN=hub", CertIssuer: "CN=ca",
		CertFingerprint: "ab:cd", TargetScope: "hub", Status: "active", Version: 1,
	}
	if err := db.CreateCredential(ctx, testTenant, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	noCert := &store.Credential{ID: "cred-2", Provider: "rockblock", Name: "Webhook secret", CredType: "webhook_secret",
		EncryptedData: []byte("x"), TargetScope: "all", Status: "active", Version: 1}
	if err := db.CreateCredential(ctx, testTenant, noCert); err != nil {
		t.Fatalf("create without cert: %v", err)
	}
	farFuture := time.Now().Add(365 * 24 * time.Hour)
	if err := db.CreateCredential(ctx, "other", &store.Credential{ID: "cred-3", Provider: "globalstar", Name: "Far", CredType: "mtls_bundle",
		EncryptedData: []byte("y"), CertNotAfter: &farFuture, TargetScope: "hub", Status: "active", Version: 1}); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetCredential(ctx, testTenant, "cred-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got.EncryptedData, blob) {
		t.Fatalf("encrypted bytes round trip: %x != %x", got.EncryptedData, blob)
	}
	if got.CertNotAfter == nil || !got.CertNotAfter.Equal(notAfter) || got.CertNotAfter.Location() != time.UTC {
		t.Fatalf("cert_not_after: %v", got.CertNotAfter)
	}
	if got.DistributedAt != nil || got.TenantID != testTenant || got.Provider != "cloudloop_mqtt" || got.Version != 1 || got.CertSubject != "CN=hub" {
		t.Fatalf("round trip: %+v", got)
	}
	if nc, _ := db.GetCredential(ctx, testTenant, "cred-2"); nc == nil || nc.CertNotAfter != nil {
		t.Fatalf("nil cert_not_after must stay nil: %+v", nc)
	}
	if _, err := db.GetCredential(ctx, "other", "cred-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant get: %v", err)
	}

	list, err := db.ListCredentials(ctx, testTenant)
	if err != nil || len(list) != 2 || list[0].ID != "cred-1" || list[1].ID != "cred-2" {
		t.Fatalf("list (ORDER BY provider, name): %+v %v", list, err)
	}

	expiring, err := db.ListExpiringCredentials(ctx, time.Now().Add(30*24*time.Hour))
	if err != nil || len(expiring) != 1 || expiring[0].ID != "cred-1" {
		t.Fatalf("expiring within 30d: %+v %v", expiring, err)
	}
	if none, _ := db.ListExpiringCredentials(ctx, time.Now()); len(none) != 0 {
		t.Fatalf("nothing expires now: %+v", none)
	}

	c.Status, c.Version, c.EncryptedData = "revoked", 2, []byte("rotated")
	if err := db.UpdateCredential(ctx, testTenant, c); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = db.GetCredential(ctx, testTenant, "cred-1")
	if got.Status != "revoked" || got.Version != 2 || string(got.EncryptedData) != "rotated" || !got.UpdatedAt.After(got.CreatedAt) {
		t.Fatalf("update not applied: %+v", got)
	}
	if still, _ := db.ListExpiringCredentials(ctx, time.Now().Add(30*24*time.Hour)); len(still) != 0 {
		t.Fatalf("revoked credential must not be listed as expiring: %+v", still)
	}
	if err := db.DeleteCredential(ctx, testTenant, "cred-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.GetCredential(ctx, testTenant, "cred-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted credential readable: %v", err)
	}
}
