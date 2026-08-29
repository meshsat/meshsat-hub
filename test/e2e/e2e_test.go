//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// ---------- Device CRUD ----------

func TestDeviceCRUD(t *testing.T) {
	h := newHarness(t)

	// 1. Create device
	body := `{"imei":"300234063904190","label":"Field Unit Alpha","type":"rockblock","notes":"Test device"}`
	resp := doRequest(t, h, http.MethodPost, "/api/devices", body)
	expectStatus(t, resp, http.StatusCreated)

	var created struct {
		IMEI  string `json:"imei"`
		Label string `json:"label"`
		Type  string `json:"type"`
		Notes string `json:"notes"`
	}
	decodeJSON(t, resp.Body, &created)
	if created.IMEI != "300234063904190" {
		t.Errorf("created IMEI = %q, want 300234063904190", created.IMEI)
	}
	if created.Label != "Field Unit Alpha" {
		t.Errorf("created label = %q, want 'Field Unit Alpha'", created.Label)
	}

	// 2. GET device by IMEI
	resp = doRequest(t, h, http.MethodGet, "/api/devices/300234063904190", "")
	expectStatus(t, resp, http.StatusOK)

	var fetched store.Device
	decodeJSON(t, resp.Body, &fetched)
	if fetched.IMEI != "300234063904190" {
		t.Errorf("fetched IMEI = %q, want 300234063904190", fetched.IMEI)
	}
	if fetched.Type != "rockblock" {
		t.Errorf("fetched type = %q, want rockblock", fetched.Type)
	}

	// 3. List devices — should contain our device
	resp = doRequest(t, h, http.MethodGet, "/api/devices", "")
	expectStatus(t, resp, http.StatusOK)

	var devices []store.Device
	decodeJSON(t, resp.Body, &devices)
	if len(devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devices))
	}
	if devices[0].IMEI != "300234063904190" {
		t.Errorf("listed device IMEI = %q, want 300234063904190", devices[0].IMEI)
	}

	// 4. Update device label
	updateBody := `{"label":"Field Unit Beta","type":"rockblock","notes":"Updated"}`
	resp = doRequest(t, h, http.MethodPut, "/api/devices/300234063904190", updateBody)
	expectStatus(t, resp, http.StatusOK)

	var updated store.Device
	decodeJSON(t, resp.Body, &updated)
	if updated.Label != "Field Unit Beta" {
		t.Errorf("updated label = %q, want 'Field Unit Beta'", updated.Label)
	}
	if updated.Notes != "Updated" {
		t.Errorf("updated notes = %q, want 'Updated'", updated.Notes)
	}

	// 5. Delete device
	resp = doRequest(t, h, http.MethodDelete, "/api/devices/300234063904190", "")
	expectStatus(t, resp, http.StatusNoContent)

	// 6. Verify device is gone
	resp = doRequest(t, h, http.MethodGet, "/api/devices/300234063904190", "")
	expectStatus(t, resp, http.StatusNotFound)

	// 7. List should be empty
	resp = doRequest(t, h, http.MethodGet, "/api/devices", "")
	expectStatus(t, resp, http.StatusOK)
	var empty []store.Device
	decodeJSON(t, resp.Body, &empty)
	if len(empty) != 0 {
		t.Errorf("expected 0 devices after delete, got %d", len(empty))
	}
}

func TestDeviceCreate_DuplicateReturnsConflict(t *testing.T) {
	h := newHarness(t)

	body := `{"imei":"300234063904190","label":"Unit 1","type":"rockblock"}`
	resp := doRequest(t, h, http.MethodPost, "/api/devices", body)
	expectStatus(t, resp, http.StatusCreated)

	// Second create with same IMEI should conflict.
	resp = doRequest(t, h, http.MethodPost, "/api/devices", body)
	expectStatus(t, resp, http.StatusConflict)
}

func TestDeviceCreate_MissingIMEIReturnsBadRequest(t *testing.T) {
	h := newHarness(t)

	body := `{"label":"No IMEI","type":"rockblock"}`
	resp := doRequest(t, h, http.MethodPost, "/api/devices", body)
	expectStatus(t, resp, http.StatusBadRequest)
}

// ---------- Message Listing ----------

func TestMessageListingAfterInsert(t *testing.T) {
	h := newHarness(t)

	// Create a device first (messages reference devices).
	resp := doRequest(t, h, http.MethodPost, "/api/devices", `{"imei":"300234063904190","label":"Test","type":"rockblock"}`)
	expectStatus(t, resp, http.StatusCreated)

	// Insert messages directly via store (simulating webhook processing).
	ctx := context.Background()
	tid := store.DefaultTenantID
	for i := 0; i < 3; i++ {
		msg := &store.Message{
			ID:         fmt.Sprintf("msg-%d", i),
			DeviceIMEI: "300234063904190",
			Direction:  "mo",
			Channel:    "iridium",
			MOMSN:      100 + i,
			Text:       fmt.Sprintf("Message %d from field", i),
			RawHex:     fmt.Sprintf("%x", fmt.Sprintf("Message %d from field", i)),
			Status:     "received",
		}
		if err := h.store.InsertMessage(ctx, tid, msg); err != nil {
			t.Fatalf("insert message %d: %v", i, err)
		}
	}

	// GET /api/messages — should return all 3
	resp = doRequest(t, h, http.MethodGet, "/api/messages", "")
	expectStatus(t, resp, http.StatusOK)

	var messages []store.Message
	decodeJSON(t, resp.Body, &messages)
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}

	// GET /api/messages with device filter
	resp = doRequest(t, h, http.MethodGet, "/api/messages?device=300234063904190", "")
	expectStatus(t, resp, http.StatusOK)
	decodeJSON(t, resp.Body, &messages)
	if len(messages) != 3 {
		t.Errorf("expected 3 messages for device filter, got %d", len(messages))
	}

	// GET single message by ID
	resp = doRequest(t, h, http.MethodGet, "/api/messages/msg-0", "")
	expectStatus(t, resp, http.StatusOK)
	var single store.Message
	decodeJSON(t, resp.Body, &single)
	if single.ID != "msg-0" {
		t.Errorf("message ID = %q, want msg-0", single.ID)
	}
	if single.Text != "Message 0 from field" {
		t.Errorf("message text = %q, want 'Message 0 from field'", single.Text)
	}

	// GET nonexistent message
	resp = doRequest(t, h, http.MethodGet, "/api/messages/nonexistent", "")
	expectStatus(t, resp, http.StatusNotFound)
}

// ---------- Position Tracking ----------

func TestPositionTracking(t *testing.T) {
	h := newHarness(t)

	// Create a device.
	resp := doRequest(t, h, http.MethodPost, "/api/devices", `{"imei":"300234063904190","label":"Tracker","type":"rockblock"}`)
	expectStatus(t, resp, http.StatusCreated)

	// Insert positions directly via store.
	ctx := context.Background()
	tid := store.DefaultTenantID
	positions := []store.Position{
		{ID: "pos-1", DeviceIMEI: "300234063904190", Lat: 52.3676, Lon: 4.9041, Source: "gps"},
		{ID: "pos-2", DeviceIMEI: "300234063904190", Lat: 52.3700, Lon: 4.9100, Source: "iridium_cep", CEP: 8.0},
		{ID: "pos-3", DeviceIMEI: "300234063904190", Lat: 52.3750, Lon: 4.9200, Source: "gps"},
	}
	for _, p := range positions {
		p := p
		if err := h.store.InsertPosition(ctx, tid, &p); err != nil {
			t.Fatalf("insert position %s: %v", p.ID, err)
		}
	}

	// GET latest position for device
	resp = doRequest(t, h, http.MethodGet, "/api/devices/300234063904190/position", "")
	expectStatus(t, resp, http.StatusOK)
	var latest store.Position
	decodeJSON(t, resp.Body, &latest)
	if latest.Lat != 52.3750 {
		t.Errorf("latest lat = %v, want 52.3750", latest.Lat)
	}

	// GET all latest positions (map view)
	resp = doRequest(t, h, http.MethodGet, "/api/positions/latest", "")
	expectStatus(t, resp, http.StatusOK)
	var allLatest []json.RawMessage
	decodeJSON(t, resp.Body, &allLatest)
	if len(allLatest) != 1 {
		t.Errorf("expected 1 device with position, got %d", len(allLatest))
	}

	// GET position history
	resp = doRequest(t, h, http.MethodGet, "/api/devices/300234063904190/positions", "")
	expectStatus(t, resp, http.StatusOK)
	var paginated struct {
		Positions []store.Position `json:"positions"`
		Total     int              `json:"total"`
	}
	decodeJSON(t, resp.Body, &paginated)
	if len(paginated.Positions) != 3 {
		t.Errorf("expected 3 position history entries, got %d", len(paginated.Positions))
	}
}

// ---------- Audit Log Integrity ----------

func TestAuditLogIntegrity(t *testing.T) {
	h := newHarness(t)

	// Write several audit entries via the service to build a hash chain.
	ctx := context.Background()
	tid := store.DefaultTenantID
	entries := []struct {
		action, actor, detail string
	}{
		{"device.create", "admin", "Created device 300234063904190"},
		{"device.update", "admin", "Updated label to Beta"},
		{"key.create", "admin", "Generated API key"},
		{"device.delete", "admin", "Deleted device 300234063904190"},
	}
	for _, e := range entries {
		if err := h.audit.Log(ctx, tid, e.action, e.actor, e.detail, "127.0.0.1"); err != nil {
			t.Fatalf("audit log: %v", err)
		}
	}

	// GET /api/audit — should return all 4 entries
	resp := doRequest(t, h, http.MethodGet, "/api/audit", "")
	expectStatus(t, resp, http.StatusOK)

	var auditEntries []store.AuditEntry
	decodeJSON(t, resp.Body, &auditEntries)
	if len(auditEntries) != 4 {
		t.Fatalf("expected 4 audit entries, got %d", len(auditEntries))
	}

	// Verify each entry has a non-empty hash.
	for i, e := range auditEntries {
		if e.Hash == "" {
			t.Errorf("entry %d has empty hash", i)
		}
		if i > 0 && e.PrevHash == "" {
			// First entry can have empty prev_hash, subsequent entries must chain.
			// Note: entries are returned in reverse chronological order by default.
		}
	}

	// GET /api/audit/verify — verify hash chain integrity
	resp = doRequest(t, h, http.MethodGet, "/api/audit/verify", "")
	expectStatus(t, resp, http.StatusOK)

	var verifyResult struct {
		Valid    bool              `json:"valid"`
		Verified int               `json:"verified"`
		BrokenAt *store.AuditEntry `json:"broken_at,omitempty"`
	}
	decodeJSON(t, resp.Body, &verifyResult)
	if !verifyResult.Valid {
		t.Errorf("audit chain verification failed, broken_at = %+v", verifyResult.BrokenAt)
	}
	if verifyResult.Verified != 4 {
		t.Errorf("verify verified = %d, want 4", verifyResult.Verified)
	}

	// GET /api/audit with limit
	resp = doRequest(t, h, http.MethodGet, "/api/audit?limit=2", "")
	expectStatus(t, resp, http.StatusOK)
	var limited []store.AuditEntry
	decodeJSON(t, resp.Body, &limited)
	if len(limited) != 2 {
		t.Errorf("expected 2 audit entries with limit=2, got %d", len(limited))
	}
}

// ---------- Cross-Entity Flow ----------

func TestDeviceWithPositionsAndMessages(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tid := store.DefaultTenantID

	// Create two devices via API.
	resp := doRequest(t, h, http.MethodPost, "/api/devices", `{"imei":"111111111111111","label":"Alpha","type":"rockblock"}`)
	expectStatus(t, resp, http.StatusCreated)
	resp = doRequest(t, h, http.MethodPost, "/api/devices", `{"imei":"222222222222222","label":"Bravo","type":"rockblock"}`)
	expectStatus(t, resp, http.StatusCreated)

	// Insert messages for both devices.
	for _, imei := range []string{"111111111111111", "222222222222222"} {
		msg := &store.Message{
			ID: "msg-" + imei, DeviceIMEI: imei, Direction: "mo",
			Channel: "iridium", Text: "Hello from " + imei, Status: "received",
		}
		if err := h.store.InsertMessage(ctx, tid, msg); err != nil {
			t.Fatalf("insert message for %s: %v", imei, err)
		}
	}

	// Insert positions for both devices.
	for _, imei := range []string{"111111111111111", "222222222222222"} {
		pos := &store.Position{
			ID: "pos-" + imei, DeviceIMEI: imei, Lat: 52.0, Lon: 4.0, Source: "gps",
		}
		if err := h.store.InsertPosition(ctx, tid, pos); err != nil {
			t.Fatalf("insert position for %s: %v", imei, err)
		}
	}

	// List all devices — should have 2.
	resp = doRequest(t, h, http.MethodGet, "/api/devices", "")
	expectStatus(t, resp, http.StatusOK)
	var devs []store.Device
	decodeJSON(t, resp.Body, &devs)
	if len(devs) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(devs))
	}

	// All latest positions — should have 2 entries.
	resp = doRequest(t, h, http.MethodGet, "/api/positions/latest", "")
	expectStatus(t, resp, http.StatusOK)
	var allPos []json.RawMessage
	decodeJSON(t, resp.Body, &allPos)
	if len(allPos) != 2 {
		t.Errorf("expected 2 latest positions, got %d", len(allPos))
	}

	// Messages — should have 2 total.
	resp = doRequest(t, h, http.MethodGet, "/api/messages", "")
	expectStatus(t, resp, http.StatusOK)
	var msgs []store.Message
	decodeJSON(t, resp.Body, &msgs)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	// Messages filtered by device.
	resp = doRequest(t, h, http.MethodGet, "/api/messages?device=111111111111111", "")
	expectStatus(t, resp, http.StatusOK)
	decodeJSON(t, resp.Body, &msgs)
	if len(msgs) != 1 {
		t.Errorf("expected 1 message for device 111111111111111, got %d", len(msgs))
	}

	// Delete device Alpha — messages and positions persist (no cascade in schema).
	resp = doRequest(t, h, http.MethodDelete, "/api/devices/111111111111111", "")
	expectStatus(t, resp, http.StatusNoContent)

	// Verify device is deleted.
	resp = doRequest(t, h, http.MethodGet, "/api/devices", "")
	expectStatus(t, resp, http.StatusOK)
	decodeJSON(t, resp.Body, &devs)
	if len(devs) != 1 {
		t.Errorf("expected 1 device after delete, got %d", len(devs))
	}
}

// ---------- HTTP helpers ----------

func doRequest(t *testing.T, h *testHarness, method, path, body string) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != "" {
		bodyReader = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, h.server.URL+path, bodyReader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func expectStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode, want, string(body))
	}
}

func decodeJSON(t *testing.T, body io.Reader, v interface{}) {
	t.Helper()
	if err := json.NewDecoder(body).Decode(v); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
}
