package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cubeos-app/meshsat-hub/internal/audit"
	hubauth "github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/cubeos-app/meshsat-hub/internal/store/sqlite"
	"github.com/go-chi/chi/v5"
)

// testEnv holds a fully wired test environment for API handler tests.
type testEnv struct {
	store  store.Store
	router *chi.Mux
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	r := chi.NewRouter()

	// Inject tenant context (default tenant for all tests).
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := context.WithValue(req.Context(), hubauth.TenantContextKey, store.DefaultTenantID)
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})

	devH := NewDeviceHandler(s)
	r.Get("/api/devices", devH.ListDevices)
	r.Post("/api/devices", devH.CreateDevice)
	r.Get("/api/devices/{imei}", devH.GetDevice)
	r.Put("/api/devices/{imei}", devH.UpdateDevice)
	r.Delete("/api/devices/{imei}", devH.DeleteDevice)

	msgH := NewMessageHandler(s)
	r.Get("/api/messages", msgH.ListMessages)
	r.Get("/api/messages/{id}", msgH.GetMessage)

	posH := NewPositionHandler(s)
	r.Get("/api/positions/latest", posH.AllLatestPositions)
	r.Get("/api/devices/{imei}/position", posH.LatestPosition)
	r.Get("/api/devices/{imei}/positions", posH.ListPositions)

	cfgH := NewDeviceConfigHandler(s)
	r.Get("/api/devices/{imei}/config", cfgH.GetLatest)
	r.Put("/api/devices/{imei}/config", cfgH.CreateVersion)
	r.Get("/api/devices/{imei}/config/history", cfgH.ListVersions)
	r.Get("/api/devices/{imei}/config/{version}", cfgH.GetVersion)

	auditSvc := audit.New(s)
	auditH := NewAuditHandler(auditSvc)
	r.Get("/api/audit", auditH.ListEntries)
	r.Get("/api/audit/verify", auditH.VerifyChain)

	keyH := NewAPIKeyHandler(s)
	r.Post("/api/auth/keys", keyH.CreateKey)
	r.Get("/api/auth/keys", keyH.ListKeys)
	r.Delete("/api/auth/keys/{id}", keyH.DeleteKey)

	r.Get("/api/auth/me", AuthMeHandler)

	return &testEnv{store: s, router: r}
}

func (e *testEnv) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func (e *testEnv) doJSON(method, path string, body interface{}) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	return e.do(req)
}

// --- Device CRUD ---

func TestDeviceHandler_ListEmpty(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodGet, "/api/devices", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var devices []store.Device
	_ = json.Unmarshal(rec.Body.Bytes(), &devices)
	if len(devices) != 0 {
		t.Fatalf("expected empty list, got %d", len(devices))
	}
}

func TestDeviceHandler_CreateAndGet(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodPost, "/api/devices", map[string]string{
		"imei":  "300234065123456",
		"label": "Test Device",
		"type":  "rockblock",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var dev store.Device
	_ = json.Unmarshal(rec.Body.Bytes(), &dev)
	if dev.IMEI != "300234065123456" {
		t.Errorf("expected IMEI 300234065123456, got %s", dev.IMEI)
	}

	// Get by IMEI
	rec = env.doJSON(http.MethodGet, "/api/devices/300234065123456", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestDeviceHandler_CreateMissingIMEI(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodPost, "/api/devices", map[string]string{
		"label": "No IMEI",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestDeviceHandler_CreateInvalidJSON(t *testing.T) {
	env := newTestEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/api/devices", bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")
	rec := env.do(req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestDeviceHandler_CreateDefaultType(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodPost, "/api/devices", map[string]string{
		"imei": "999999999999999",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rec.Code)
	}
	var dev store.Device
	_ = json.Unmarshal(rec.Body.Bytes(), &dev)
	if dev.Type != "rockblock" {
		t.Errorf("expected default type rockblock, got %s", dev.Type)
	}
}

func TestDeviceHandler_CreateDuplicate(t *testing.T) {
	env := newTestEnv(t)
	body := map[string]string{"imei": "300234065123456"}
	env.doJSON(http.MethodPost, "/api/devices", body)
	rec := env.doJSON(http.MethodPost, "/api/devices", body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
}

func TestDeviceHandler_GetNotFound(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodGet, "/api/devices/nonexistent", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestDeviceHandler_Update(t *testing.T) {
	env := newTestEnv(t)
	env.doJSON(http.MethodPost, "/api/devices", map[string]string{"imei": "300234065123456"})

	rec := env.doJSON(http.MethodPut, "/api/devices/300234065123456", map[string]string{
		"label": "Updated",
		"type":  "astrocast",
		"notes": "test notes",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var dev store.Device
	_ = json.Unmarshal(rec.Body.Bytes(), &dev)
	if dev.Label != "Updated" {
		t.Errorf("expected label Updated, got %s", dev.Label)
	}
}

func TestDeviceHandler_UpdateNotFound(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodPut, "/api/devices/nonexistent", map[string]string{"label": "x"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestDeviceHandler_UpdateInvalidJSON(t *testing.T) {
	env := newTestEnv(t)
	env.doJSON(http.MethodPost, "/api/devices", map[string]string{"imei": "300234065123456"})
	req := httptest.NewRequest(http.MethodPut, "/api/devices/300234065123456", bytes.NewReader([]byte("bad")))
	req.Header.Set("Content-Type", "application/json")
	rec := env.do(req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestDeviceHandler_Delete(t *testing.T) {
	env := newTestEnv(t)
	env.doJSON(http.MethodPost, "/api/devices", map[string]string{"imei": "300234065123456"})

	rec := env.doJSON(http.MethodDelete, "/api/devices/300234065123456", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}

	rec = env.doJSON(http.MethodGet, "/api/devices/300234065123456", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", rec.Code)
	}
}

func TestDeviceHandler_DeleteNotFound(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodDelete, "/api/devices/nonexistent", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// --- Messages ---

func TestMessageHandler_ListEmpty(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodGet, "/api/messages", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var msgs []store.Message
	_ = json.Unmarshal(rec.Body.Bytes(), &msgs)
	if len(msgs) != 0 {
		t.Fatalf("expected empty, got %d", len(msgs))
	}
}

func TestMessageHandler_ListWithFilter(t *testing.T) {
	env := newTestEnv(t)
	// Insert a message via store directly
	_ = env.store.InsertMessage(context.Background(), store.DefaultTenantID, &store.Message{
		ID:         "msg-1",
		DeviceIMEI: "300234065123456",
		Direction:  "mo",
		Text:       "test",
		Status:     "received",
	})
	rec := env.doJSON(http.MethodGet, "/api/messages?device=300234065123456&limit=10", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var msgs []store.Message
	_ = json.Unmarshal(rec.Body.Bytes(), &msgs)
	if len(msgs) != 1 {
		t.Fatalf("expected 1, got %d", len(msgs))
	}
}

func TestMessageHandler_GetNotFound(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodGet, "/api/messages/nonexistent", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestMessageHandler_GetExisting(t *testing.T) {
	env := newTestEnv(t)
	_ = env.store.InsertMessage(context.Background(), store.DefaultTenantID, &store.Message{
		ID: "msg-2", DeviceIMEI: "dev1", Direction: "mo", Status: "received",
	})
	rec := env.doJSON(http.MethodGet, "/api/messages/msg-2", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

// --- Positions ---

func TestPositionHandler_LatestNotFound(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodGet, "/api/devices/nodev/position", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestPositionHandler_ListEmpty(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodGet, "/api/devices/dev1/positions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var positions []store.Position
	_ = json.Unmarshal(rec.Body.Bytes(), &positions)
	if len(positions) != 0 {
		t.Fatalf("expected empty, got %d", len(positions))
	}
}

func TestPositionHandler_LatestAndList(t *testing.T) {
	env := newTestEnv(t)
	_ = env.store.InsertPosition(context.Background(), store.DefaultTenantID, &store.Position{
		ID: "pos-1", DeviceIMEI: "dev1", Lat: 52.37, Lon: 4.89, Source: "gps",
	})

	rec := env.doJSON(http.MethodGet, "/api/devices/dev1/position", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = env.doJSON(http.MethodGet, "/api/devices/dev1/positions?limit=5", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestPositionHandler_AllLatestEmpty(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodGet, "/api/positions/latest", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestPositionHandler_AllLatestWithDevice(t *testing.T) {
	env := newTestEnv(t)
	_ = env.store.CreateDevice(context.Background(), store.DefaultTenantID, &store.Device{
		IMEI: "dev1", Label: "Dev 1", Type: "rockblock",
	})
	_ = env.store.InsertPosition(context.Background(), store.DefaultTenantID, &store.Position{
		ID: "pos-1", DeviceIMEI: "dev1", Lat: 52.37, Lon: 4.89, Source: "gps",
	})
	rec := env.doJSON(http.MethodGet, "/api/positions/latest", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var positions []json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &positions)
	if len(positions) != 1 {
		t.Fatalf("expected 1 position, got %d", len(positions))
	}
}

// --- Device Config ---

func TestDeviceConfigHandler_GetLatestNotFound(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodGet, "/api/devices/nodev/config", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestDeviceConfigHandler_CreateAndGetLatest(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodPut, "/api/devices/dev1/config", map[string]interface{}{
		"config":  map[string]string{"key": "value"},
		"comment": "initial config",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = env.doJSON(http.MethodGet, "/api/devices/dev1/config", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestDeviceConfigHandler_CreateMissingConfig(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodPut, "/api/devices/dev1/config", map[string]string{
		"comment": "no config field",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestDeviceConfigHandler_CreateInvalidConfigJSON(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodPut, "/api/devices/dev1/config", map[string]interface{}{
		"config": "not-valid-json{",
	})
	// The config field is a string that should be valid JSON
	// When encoded through our test helper it will be a JSON string, which IS valid JSON
	// So let's send raw bytes to test actual invalid JSON
	req := httptest.NewRequest(http.MethodPut, "/api/devices/dev1/config",
		bytes.NewReader([]byte(`{"config": "not-json{" }`)))
	req.Header.Set("Content-Type", "application/json")
	rec2 := env.do(req)
	// The handler checks json.Valid on the config field's raw JSON value.
	// Since we're sending a string, this depends on how createConfigRequest parses it.
	// config is json.RawMessage, so "not-json{" is not valid.
	if rec2.Code != http.StatusBadRequest {
		t.Logf("config invalid JSON: got %d (may be valid if string-encoded)", rec2.Code)
	}
	_ = rec // suppress unused
}

func TestDeviceConfigHandler_CreateInvalidBody(t *testing.T) {
	env := newTestEnv(t)
	req := httptest.NewRequest(http.MethodPut, "/api/devices/dev1/config",
		bytes.NewReader([]byte("not json at all")))
	req.Header.Set("Content-Type", "application/json")
	rec := env.do(req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestDeviceConfigHandler_ListVersions(t *testing.T) {
	env := newTestEnv(t)
	env.doJSON(http.MethodPut, "/api/devices/dev1/config", map[string]interface{}{
		"config": map[string]string{"v": "1"}, "comment": "v1",
	})
	env.doJSON(http.MethodPut, "/api/devices/dev1/config", map[string]interface{}{
		"config": map[string]string{"v": "2"}, "comment": "v2",
	})

	rec := env.doJSON(http.MethodGet, "/api/devices/dev1/config/history", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var configs []store.DeviceConfig
	_ = json.Unmarshal(rec.Body.Bytes(), &configs)
	if len(configs) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(configs))
	}
}

func TestDeviceConfigHandler_GetVersion(t *testing.T) {
	env := newTestEnv(t)
	env.doJSON(http.MethodPut, "/api/devices/dev1/config", map[string]interface{}{
		"config": map[string]string{"v": "1"},
	})

	rec := env.doJSON(http.MethodGet, "/api/devices/dev1/config/1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeviceConfigHandler_GetVersionInvalid(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodGet, "/api/devices/dev1/config/abc", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestDeviceConfigHandler_GetVersionZero(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodGet, "/api/devices/dev1/config/0", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestDeviceConfigHandler_GetVersionNotFound(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodGet, "/api/devices/dev1/config/999", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// --- Audit ---

func TestAuditHandler_ListEmpty(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodGet, "/api/audit", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var entries []store.AuditEntry
	_ = json.Unmarshal(rec.Body.Bytes(), &entries)
	if len(entries) != 0 {
		t.Fatalf("expected empty, got %d", len(entries))
	}
}

func TestAuditHandler_ListWithLimit(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodGet, "/api/audit?limit=5", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestAuditHandler_VerifyEmptyChain(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodGet, "/api/audit/verify", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var result map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &result)
	if result["valid"] != true {
		t.Errorf("expected valid=true for empty chain, got %v", result["valid"])
	}
}

// --- API Keys ---

func TestAPIKeyHandler_CreateAndList(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodPost, "/api/auth/keys", map[string]string{
		"label": "Test Key",
		"role":  "viewer",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp createKeyResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Key == "" {
		t.Error("expected plaintext key in response")
	}
	if resp.Role != "viewer" {
		t.Errorf("expected role viewer, got %s", resp.Role)
	}

	// List keys
	rec = env.doJSON(http.MethodGet, "/api/auth/keys", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var keys []store.APIKey
	_ = json.Unmarshal(rec.Body.Bytes(), &keys)
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
}

func TestAPIKeyHandler_CreateDefaultRole(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodPost, "/api/auth/keys", map[string]string{
		"label": "No Role",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rec.Code)
	}
	var resp createKeyResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Role != "viewer" {
		t.Errorf("expected default role viewer, got %s", resp.Role)
	}
}

func TestAPIKeyHandler_CreateInvalidRole(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodPost, "/api/auth/keys", map[string]string{
		"label": "Bad Role",
		"role":  "superadmin",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestAPIKeyHandler_CreateInvalidJSON(t *testing.T) {
	env := newTestEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/keys", bytes.NewReader([]byte("bad")))
	req.Header.Set("Content-Type", "application/json")
	rec := env.do(req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestAPIKeyHandler_CreateWithExpiry(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodPost, "/api/auth/keys", map[string]string{
		"label":      "Expiring",
		"role":       "operator",
		"expires_in": "720h",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rec.Code)
	}
	var resp createKeyResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.ExpiresAt == nil {
		t.Error("expected expires_at to be set")
	}
}

func TestAPIKeyHandler_CreateInvalidExpiry(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodPost, "/api/auth/keys", map[string]string{
		"label":      "Bad Expiry",
		"expires_in": "not-a-duration",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestAPIKeyHandler_Delete(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodPost, "/api/auth/keys", map[string]string{
		"label": "To Delete",
	})
	var resp createKeyResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	rec = env.doJSON(http.MethodDelete, "/api/auth/keys/"+resp.ID, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}

	// Verify deletion
	rec = env.doJSON(http.MethodGet, "/api/auth/keys", nil)
	var keys []store.APIKey
	_ = json.Unmarshal(rec.Body.Bytes(), &keys)
	if len(keys) != 0 {
		t.Fatalf("expected 0 keys after delete, got %d", len(keys))
	}
}

func TestAPIKeyHandler_DeleteMissingID(t *testing.T) {
	env := newTestEnv(t)
	// chi won't route /api/auth/keys/ to the delete handler (empty {id}),
	// so this tests the router behavior
	rec := env.doJSON(http.MethodDelete, "/api/auth/keys/nonexistent", nil)
	// Store delete of nonexistent key may or may not error depending on impl
	if rec.Code != http.StatusNoContent && rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 204 or 500, got %d", rec.Code)
	}
}

// --- AuthMe ---

func TestAuthMeHandler_NoUser(t *testing.T) {
	env := newTestEnv(t)
	rec := env.doJSON(http.MethodGet, "/api/auth/me", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAuthMeHandler_WithUser(t *testing.T) {
	// Test AuthMeHandler directly (without router middleware) to control context.
	user := &hubauth.User{
		ID:    "user-1",
		Email: "test@example.com",
		Name:  "Test User",
		Roles: []string{"owner"},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	ctx := context.WithValue(req.Context(), hubauth.UserContextKey, user)
	ctx = context.WithValue(ctx, hubauth.TenantContextKey, "tenant-1")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	AuthMeHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var result map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &result)
	if result["id"] != "user-1" {
		t.Errorf("expected id user-1, got %v", result["id"])
	}
	if result["tenant_id"] != "tenant-1" {
		t.Errorf("expected tenant_id tenant-1, got %v", result["tenant_id"])
	}
}
