package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

const (
	tenant = "default"
	imei   = "300234063904190"
)

func TestDevices(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	d := &store.Device{IMEI: imei, Label: "Field Unit 1", Type: "rockblock", Notes: "n"}
	if err := db.CreateDevice(ctx, tenant, d); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := db.GetDevice(ctx, tenant, imei)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Label != "Field Unit 1" || got.Type != "rockblock" || got.Notes != "n" {
		t.Errorf("get: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Error("created_at/updated_at should be set")
	}
	if !got.LastSeen.IsZero() {
		t.Errorf("last_seen before touch should be zero, got %v", got.LastSeen)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Errorf("created_at not UTC: %v", got.CreatedAt.Location())
	}

	// bridge_id is NULL for every device created here: reads must not fail.
	if _, err := db.rawDB.ExecContext(ctx, "UPDATE devices SET bridge_id = NULL WHERE imei = $1", imei); err != nil {
		t.Fatal(err)
	}

	got.Label = "Renamed"
	if err := db.UpdateDevice(ctx, tenant, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := db.TouchDeviceLastSeen(ctx, tenant, imei); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if err := db.CreateDevice(ctx, tenant, &store.Device{IMEI: "100000000000001", Label: "Aardvark", Type: "globalstar"}); err != nil {
		t.Fatalf("create second: %v", err)
	}
	all, err := db.ListDevices(ctx, tenant)
	if err != nil || len(all) != 2 {
		t.Fatalf("list: %v %+v", err, all)
	}
	if all[0].Label != "Aardvark" || all[1].Label != "Renamed" {
		t.Errorf("list ordering by label: %+v", all)
	}
	if all[1].LastSeen.IsZero() || time.Since(all[1].LastSeen) > time.Minute {
		t.Errorf("last_seen not touched: %v", all[1].LastSeen)
	}
	if !all[1].UpdatedAt.After(all[1].CreatedAt) && !all[1].UpdatedAt.Equal(all[1].CreatedAt) {
		t.Errorf("updated_at %v before created_at %v", all[1].UpdatedAt, all[1].CreatedAt)
	}

	// Tenant scoping.
	if _, err := db.GetDevice(ctx, "other", imei); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("other tenant get: %v", err)
	}
	if other, err := db.ListDevices(ctx, "other"); err != nil || len(other) != 0 {
		t.Errorf("other tenant list: %v %+v", err, other)
	}
	if err := db.DeleteDevice(ctx, "other", imei); err != nil {
		t.Fatalf("delete other tenant: %v", err)
	}
	if _, err := db.GetDevice(ctx, tenant, imei); err != nil {
		t.Fatalf("delete from other tenant must be a no-op: %v", err)
	}

	if err := db.DeleteDevice(ctx, tenant, imei); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.GetDevice(ctx, tenant, imei); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("get after delete: %v", err)
	}
	// Updating/touching a missing device is not an error (MariaDB semantics).
	if err := db.UpdateDevice(ctx, tenant, &store.Device{IMEI: "nope"}); err != nil {
		t.Errorf("update missing: %v", err)
	}
	if err := db.TouchDeviceLastSeen(ctx, tenant, "nope"); err != nil {
		t.Errorf("touch missing: %v", err)
	}
}

func TestMessages(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	msg := &store.Message{DeviceIMEI: imei, Direction: "mo", Channel: "iridium", MOMSN: 42, Text: "Hello from field",
		RawHex: "48656c6c6f", Compressed: true, Status: "received", Lat: 52.1, Lon: 4.9}
	if err := db.InsertMessage(ctx, tenant, msg); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if msg.ID == "" || msg.ID[:4] != "msg-" {
		t.Errorf("ID should be generated with msg- prefix, got %q", msg.ID)
	}
	time.Sleep(5 * time.Millisecond)
	second := &store.Message{ID: "mt-1", DeviceIMEI: imei, Direction: "mt", Channel: "iridium", Text: "reply", Status: "queued"}
	if err := db.InsertMessage(ctx, tenant, second); err != nil {
		t.Fatalf("insert second: %v", err)
	}
	if err := db.InsertMessage(ctx, tenant, &store.Message{ID: "other-dev", DeviceIMEI: "999", Direction: "mo", Channel: "iridium", Status: "received"}); err != nil {
		t.Fatalf("insert other device: %v", err)
	}

	msgs, err := db.ListMessages(ctx, tenant, imei, 10)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("list: %v %+v", err, msgs)
	}
	if msgs[0].ID != "mt-1" || msgs[1].ID != msg.ID {
		t.Errorf("list must be newest first: %+v", msgs)
	}
	if m := msgs[1]; m.MOMSN != 42 || !m.Compressed || m.Lat != 52.1 || m.Lon != 4.9 || m.RawHex != "48656c6c6f" {
		t.Errorf("round trip: %+v", m)
	}
	if all, err := db.ListMessages(ctx, tenant, "", 0); err != nil || len(all) != 3 {
		t.Errorf("list all (no device, no limit): %v %d", err, len(all))
	}
	if lim, err := db.ListMessages(ctx, tenant, "", 1); err != nil || len(lim) != 1 || lim[0].ID != "other-dev" {
		t.Errorf("list limit 1: %v %+v", err, lim)
	}
	if other, err := db.ListMessages(ctx, "other", "", 0); err != nil || len(other) != 0 {
		t.Errorf("other tenant: %v %+v", err, other)
	}

	got, err := db.GetMessage(ctx, tenant, msg.ID)
	if err != nil || got.MOMSN != 42 || got.CreatedAt.IsZero() {
		t.Fatalf("get: %v %+v", err, got)
	}
	if _, err := db.GetMessage(ctx, "other", msg.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("get other tenant: %v", err)
	}
	if _, err := db.GetMessage(ctx, tenant, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("get missing: %v", err)
	}

	if err := db.UpdateMessageStatus(ctx, tenant, msg.ID, "failed", "boom"); err != nil {
		t.Fatalf("update status: %v", err)
	}
	got, _ = db.GetMessage(ctx, tenant, msg.ID)
	if got.Status != "failed" || got.Error != "boom" {
		t.Errorf("status: %+v", got)
	}
}

func TestMessageDuplicate(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

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

func TestScheduledMessages(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	due := &store.Message{ID: "s-due", DeviceIMEI: imei, Direction: "mt", Channel: "iridium", Text: "later", Status: "scheduled", ScheduledAt: now.Add(-time.Minute)}
	earlier := &store.Message{ID: "s-earlier", DeviceIMEI: imei, Direction: "mt", Channel: "iridium", Text: "first", Status: "scheduled", ScheduledAt: now.Add(-2 * time.Hour)}
	future := &store.Message{ID: "s-future", DeviceIMEI: imei, Direction: "mt", Channel: "iridium", Text: "future", Status: "scheduled", ScheduledAt: now.Add(time.Hour)}
	sent := &store.Message{ID: "s-sent", DeviceIMEI: imei, Direction: "mt", Channel: "iridium", Text: "sent", Status: "sent", ScheduledAt: now.Add(-time.Minute)}
	plain := &store.Message{ID: "s-plain", DeviceIMEI: imei, Direction: "mt", Channel: "iridium", Text: "plain", Status: "scheduled"}
	for _, m := range []*store.Message{due, earlier, future, sent, plain} {
		if err := db.InsertMessage(ctx, "tenant-x", m); err != nil {
			t.Fatalf("insert %s: %v", m.ID, err)
		}
	}
	got, err := db.ListScheduledMessages(ctx, now, 10)
	if err != nil {
		t.Fatalf("list scheduled: %v", err)
	}
	if len(got) != 2 || got[0].ID != "s-earlier" || got[1].ID != "s-due" {
		t.Fatalf("want [s-earlier s-due] ordered by scheduled_at, got %+v", got)
	}
	if !got[1].ScheduledAt.Equal(now.Add(-time.Minute)) {
		t.Errorf("scheduled_at round trip: %v vs %v", got[1].ScheduledAt, now.Add(-time.Minute))
	}
	if got[1].ScheduledAt.Location() != time.UTC {
		t.Errorf("scheduled_at not UTC")
	}
	lim, err := db.ListScheduledMessages(ctx, now, 1)
	if err != nil || len(lim) != 1 || lim[0].ID != "s-earlier" {
		t.Errorf("limit 1: %v %+v", err, lim)
	}
}

func TestWebhooksAndDeliveryLogs(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	wh := &store.WebhookConfig{ID: "wh-1", URL: "https://example.com/hook", Secret: "s3cret", Events: []string{"mo", "sos"}, MaxRetries: 5, TimeoutSec: 7, Enabled: true}
	if err := db.SaveWebhook(ctx, tenant, wh); err != nil {
		t.Fatalf("save: %v", err)
	}
	wh.URL = "https://example.com/hook2"
	wh.Events = []string{"mo"}
	wh.Enabled = false
	if err := db.SaveWebhook(ctx, tenant, wh); err != nil {
		t.Fatalf("save (upsert): %v", err)
	}
	if err := db.SaveWebhook(ctx, tenant, &store.WebhookConfig{ID: "wh-nil", URL: "https://example.com/nil", Events: nil, Enabled: true}); err != nil {
		t.Fatalf("save nil events: %v", err)
	}
	all, err := db.ListWebhooks(ctx, tenant)
	if err != nil || len(all) != 2 {
		t.Fatalf("list: %v %+v", err, all)
	}
	byID := map[string]store.WebhookConfig{}
	for _, w := range all {
		byID[w.ID] = w
	}
	w1 := byID["wh-1"]
	if w1.URL != "https://example.com/hook2" || len(w1.Events) != 1 || w1.Events[0] != "mo" || w1.Enabled ||
		w1.MaxRetries != 5 || w1.TimeoutSec != 7 || w1.Secret != "s3cret" || w1.CreatedAt.IsZero() {
		t.Errorf("upsert result: %+v", w1)
	}
	if wn := byID["wh-nil"]; len(wn.Events) != 0 {
		t.Errorf("nil events should round-trip as empty: %+v", wn)
	}
	if other, err := db.ListWebhooks(ctx, "other"); err != nil || len(other) != 0 {
		t.Errorf("other tenant: %v %+v", err, other)
	}

	l := &store.DeliveryLog{WebhookID: "wh-1", Event: "mo", DeviceIMEI: "x", StatusCode: 200, Attempt: 1}
	if err := db.InsertDeliveryLog(ctx, tenant, l); err != nil {
		t.Fatalf("delivery log: %v", err)
	}
	if l.ID == "" || l.ID[:3] != "dl-" {
		t.Errorf("delivery log ID: %q", l.ID)
	}
	time.Sleep(5 * time.Millisecond)
	if err := db.InsertDeliveryLog(ctx, tenant, &store.DeliveryLog{ID: "dl-2", WebhookID: "wh-1", Event: "sos", StatusCode: 500, Error: "timeout", Attempt: 2}); err != nil {
		t.Fatalf("delivery log 2: %v", err)
	}
	logs, err := db.ListDeliveryLogs(ctx, tenant, 10)
	if err != nil || len(logs) != 2 {
		t.Fatalf("delivery logs: %v %+v", err, logs)
	}
	if logs[0].ID != "dl-2" || logs[0].Error != "timeout" || logs[0].StatusCode != 500 || logs[1].StatusCode != 200 {
		t.Errorf("delivery logs order/content: %+v", logs)
	}
	if lim, err := db.ListDeliveryLogs(ctx, tenant, 1); err != nil || len(lim) != 1 {
		t.Errorf("delivery logs limit: %v %d", err, len(lim))
	}
	if other, err := db.ListDeliveryLogs(ctx, "other", 10); err != nil || len(other) != 0 {
		t.Errorf("delivery logs other tenant: %v %d", err, len(other))
	}

	if err := db.DeleteWebhook(ctx, "other", "wh-1"); err != nil {
		t.Fatalf("delete other tenant: %v", err)
	}
	if all2, _ := db.ListWebhooks(ctx, tenant); len(all2) != 2 {
		t.Errorf("delete from other tenant must be a no-op: %d", len(all2))
	}
	if err := db.DeleteWebhook(ctx, tenant, "wh-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if all2, _ := db.ListWebhooks(ctx, tenant); len(all2) != 1 || all2[0].ID != "wh-nil" {
		t.Errorf("after delete: %+v", all2)
	}
}

func TestPositions(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if _, err := db.LatestPosition(ctx, tenant, imei); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("latest on empty: %v", err)
	}
	p1 := &store.Position{DeviceIMEI: imei, Lat: 52.3676, Lon: 4.9041, Alt: 3, Speed: 1.5, Heading: 90, Sats: 8, Source: "iridium_cep", CEP: 10.0}
	if err := db.InsertPosition(ctx, tenant, p1); err != nil {
		t.Fatalf("insert p1: %v", err)
	}
	if p1.ID == "" || p1.ID[:4] != "pos-" {
		t.Errorf("position ID: %q", p1.ID)
	}
	time.Sleep(20 * time.Millisecond)
	p2 := &store.Position{ID: "p2", DeviceIMEI: imei, Lat: 52.37, Lon: 4.91, Source: "gps"}
	if err := db.InsertPosition(ctx, tenant, p2); err != nil {
		t.Fatalf("insert p2: %v", err)
	}
	if err := db.InsertPosition(ctx, "other", &store.Position{ID: "p-other", DeviceIMEI: imei, Lat: 1, Lon: 1, Source: "gps"}); err != nil {
		t.Fatalf("insert other tenant: %v", err)
	}

	latest, err := db.LatestPosition(ctx, tenant, imei)
	if err != nil || latest.ID != "p2" || latest.Source != "gps" {
		t.Fatalf("latest: %v %+v", err, latest)
	}
	all, err := db.ListPositions(ctx, tenant, imei, 10)
	if err != nil || len(all) != 2 || all[0].ID != "p2" {
		t.Fatalf("list: %v %+v", err, all)
	}
	if got := all[1]; got.Alt != 3 || got.Speed != 1.5 || got.Heading != 90 || got.Sats != 8 || got.CEP != 10.0 || got.CreatedAt.IsZero() {
		t.Errorf("round trip: %+v", got)
	}
	if lim, _ := db.ListPositions(ctx, tenant, imei, 1); len(lim) != 1 {
		t.Errorf("list limit: %d", len(lim))
	}

	// Range: bounded both sides, page size 1.
	ranged, total, err := db.ListPositionsRange(ctx, tenant, imei, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), 1, 0)
	if err != nil || total != 2 || len(ranged) != 1 || ranged[0].ID != "p2" {
		t.Errorf("range: err=%v total=%d %+v", err, total, ranged)
	}
	// Offset into the second page.
	ranged, total, err = db.ListPositionsRange(ctx, tenant, imei, time.Time{}, time.Time{}, 1, 1)
	if err != nil || total != 2 || len(ranged) != 1 || ranged[0].ID != p1.ID {
		t.Errorf("range offset: err=%v total=%d %+v", err, total, ranged)
	}
	// Unbounded, no limit.
	ranged, total, err = db.ListPositionsRange(ctx, tenant, imei, time.Time{}, time.Time{}, 0, 0)
	if err != nil || total != 2 || len(ranged) != 2 {
		t.Errorf("range unbounded: err=%v total=%d n=%d", err, total, len(ranged))
	}
	// Only "to" bound, in the past: nothing.
	ranged, total, err = db.ListPositionsRange(ctx, tenant, imei, time.Time{}, time.Now().Add(-time.Hour), 0, 0)
	if err != nil || total != 0 || len(ranged) != 0 {
		t.Errorf("range past: err=%v total=%d n=%d", err, total, len(ranged))
	}
	// Only "from" bound, in the future: nothing.
	ranged, total, err = db.ListPositionsRange(ctx, tenant, imei, time.Now().Add(time.Hour), time.Time{}, 0, 0)
	if err != nil || total != 0 || len(ranged) != 0 {
		t.Errorf("range future: err=%v total=%d n=%d", err, total, len(ranged))
	}
	// From bound between p1 and p2 keeps only p2.
	ranged, total, err = db.ListPositionsRange(ctx, tenant, imei, all[1].CreatedAt.Add(time.Millisecond), time.Time{}, 0, 0)
	if err != nil || total != 1 || len(ranged) != 1 || ranged[0].ID != "p2" {
		t.Errorf("range from mid: err=%v total=%d %+v", err, total, ranged)
	}
	// Tenant scoping.
	if o, total, err := db.ListPositionsRange(ctx, "other", imei, time.Time{}, time.Time{}, 0, 0); err != nil || total != 1 || len(o) != 1 || o[0].ID != "p-other" {
		t.Errorf("range other tenant: err=%v total=%d %+v", err, total, o)
	}
}

func TestAuditLog(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if _, err := db.GetLatestAuditEntry(ctx, tenant); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("latest on empty: %v", err)
	}
	e1 := &store.AuditEntry{Action: "login", Actor: "admin", IP: "1.2.3.4", Hash: "h1"}
	if err := db.InsertAuditEntry(ctx, tenant, e1); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if e1.ID == "" || e1.ID[:4] != "aud-" {
		t.Errorf("audit ID: %q", e1.ID)
	}
	time.Sleep(20 * time.Millisecond)
	if err := db.InsertAuditEntry(ctx, tenant, &store.AuditEntry{ID: "a2", Action: "device.create", Actor: "admin", Detail: "IMEI=1", PrevHash: "h1", Hash: "h2"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db.InsertAuditEntry(ctx, "other", &store.AuditEntry{ID: "a-other", Action: "x", Hash: "hx"}); err != nil {
		t.Fatalf("insert other: %v", err)
	}

	entries, err := db.ListAuditEntries(ctx, tenant, 10)
	if err != nil || len(entries) != 2 || entries[0].ID != "a2" || entries[1].ID != e1.ID {
		t.Fatalf("list: %v %+v", err, entries)
	}
	if e := entries[0]; e.Detail != "IMEI=1" || e.PrevHash != "h1" || e.CreatedAt.IsZero() || e.CreatedAt.Location() != time.UTC {
		t.Errorf("round trip: %+v", e)
	}
	if lim, _ := db.ListAuditEntries(ctx, tenant, 1); len(lim) != 1 || lim[0].ID != "a2" {
		t.Errorf("list limit: %+v", lim)
	}
	latest, err := db.GetLatestAuditEntry(ctx, tenant)
	if err != nil || latest.Hash != "h2" {
		t.Fatalf("latest: %v %+v", err, latest)
	}
	if o, err := db.GetLatestAuditEntry(ctx, "other"); err != nil || o.Hash != "hx" {
		t.Errorf("latest other tenant: %v %+v", err, o)
	}

	cut := entries[0].CreatedAt // strictly before a2 -> only e1
	old, err := db.ListAuditEntriesBefore(ctx, tenant, cut, 10)
	if err != nil || len(old) != 1 || old[0].ID != e1.ID {
		t.Fatalf("before: %v %+v", err, old)
	}
	if none, err := db.ListAuditEntriesBefore(ctx, tenant, time.Now().Add(time.Hour), 0); err != nil || len(none) != 2 {
		t.Errorf("before no limit: %v %d", err, len(none))
	}
	if lim, err := db.ListAuditEntriesBefore(ctx, tenant, time.Now().Add(time.Hour), 1); err != nil || len(lim) != 1 {
		t.Errorf("before limit 1: %v %d", err, len(lim))
	}

	n, err := db.DeleteAuditEntriesBefore(ctx, tenant, cut)
	if err != nil || n != 1 {
		t.Fatalf("delete before: %v %d", err, n)
	}
	n, err = db.DeleteAuditEntriesBefore(ctx, tenant, time.Now().Add(time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("delete rest: %v %d", err, n)
	}
	if rest, _ := db.ListAuditEntries(ctx, tenant, 10); len(rest) != 0 {
		t.Errorf("after delete: %+v", rest)
	}
	if o, _ := db.ListAuditEntries(ctx, "other", 10); len(o) != 1 {
		t.Errorf("delete must be tenant scoped: %+v", o)
	}
}

func TestDeviceConfigVersioning(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if _, err := db.GetDeviceConfigLatest(ctx, tenant, imei); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("latest on empty: %v", err)
	}
	c1 := &store.DeviceConfig{DeviceIMEI: imei, Config: `{"reporting_interval":60}`, Author: "user-1", Comment: "Initial"}
	if err := db.CreateDeviceConfig(ctx, tenant, c1); err != nil {
		t.Fatalf("create v1: %v", err)
	}
	if c1.Version != 1 || c1.ID == "" || c1.ID[:4] != "cfg-" {
		t.Errorf("v1: %+v", c1)
	}
	c2 := &store.DeviceConfig{ID: "cfg-2", DeviceIMEI: imei, Config: `{"reporting_interval":30}`, Author: "user-1", Comment: "Faster"}
	if err := db.CreateDeviceConfig(ctx, tenant, c2); err != nil {
		t.Fatalf("create v2: %v", err)
	}
	if c2.Version != 2 {
		t.Errorf("version: %d", c2.Version)
	}
	// Versions are per device + tenant.
	cOther := &store.DeviceConfig{ID: "cfg-other", DeviceIMEI: imei, Config: `{}`}
	if err := db.CreateDeviceConfig(ctx, "other", cOther); err != nil || cOther.Version != 1 {
		t.Errorf("other tenant v1: %v %+v", err, cOther)
	}
	cDev := &store.DeviceConfig{ID: "cfg-dev", DeviceIMEI: "999", Config: `{}`}
	if err := db.CreateDeviceConfig(ctx, tenant, cDev); err != nil || cDev.Version != 1 {
		t.Errorf("other device v1: %v %+v", err, cDev)
	}

	latest, err := db.GetDeviceConfigLatest(ctx, tenant, imei)
	if err != nil || latest.Version != 2 || latest.Comment != "Faster" || latest.CreatedAt.IsZero() {
		t.Fatalf("latest: %v %+v", err, latest)
	}
	v1, err := db.GetDeviceConfigVersion(ctx, tenant, imei, 1)
	if err != nil || v1.Comment != "Initial" || v1.Config != `{"reporting_interval":60}` || v1.Author != "user-1" {
		t.Fatalf("v1: %v %+v", err, v1)
	}
	if _, err := db.GetDeviceConfigVersion(ctx, tenant, imei, 3); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("missing version: %v", err)
	}
	versions, err := db.ListDeviceConfigVersions(ctx, tenant, imei, 10)
	if err != nil || len(versions) != 2 || versions[0].Version != 2 || versions[1].Version != 1 {
		t.Fatalf("versions: %v %+v", err, versions)
	}
	if all, err := db.ListDeviceConfigVersions(ctx, tenant, imei, 0); err != nil || len(all) != 2 {
		t.Errorf("versions no limit: %v %d", err, len(all))
	}
	if lim, err := db.ListDeviceConfigVersions(ctx, tenant, imei, 1); err != nil || len(lim) != 1 || lim[0].Version != 2 {
		t.Errorf("versions limit 1: %v %+v", err, lim)
	}
	if o, err := db.GetDeviceConfigLatest(ctx, "other", imei); err != nil || o.Version != 1 || o.ID != "cfg-other" {
		t.Errorf("other tenant latest: %v %+v", err, o)
	}
	if _, err := db.GetDeviceConfigLatest(ctx, "nobody", imei); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("unknown tenant must not read the config: %v", err)
	}
}

func TestSystemConfig(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if _, err := db.GetSystemConfig(ctx, "missing-key"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("missing key: %v", err)
	}
	if err := db.SetSystemConfig(ctx, "bridge_ca_cert", "-----BEGIN-----"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := db.SetSystemConfig(ctx, "bridge_ca_cert", "-----BEGIN v2-----"); err != nil {
		t.Fatalf("set (upsert): %v", err)
	}
	if err := db.SetSystemConfig(ctx, "empty", ""); err != nil {
		t.Fatalf("set empty: %v", err)
	}
	v, err := db.GetSystemConfig(ctx, "bridge_ca_cert")
	if err != nil || v != "-----BEGIN v2-----" {
		t.Fatalf("get: %v %q", err, v)
	}
	if v, err := db.GetSystemConfig(ctx, "empty"); err != nil || v != "" {
		t.Errorf("get empty: %v %q", err, v)
	}
	var n int
	if err := db.rawDB.QueryRowContext(ctx, "SELECT count(*) FROM system_config").Scan(&n); err != nil || n != 2 {
		t.Errorf("upsert must not create a second row: %d %v", n, err)
	}
}
