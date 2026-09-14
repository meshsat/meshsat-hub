package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/bridge"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// mockBridgeStore is a minimal mock for bridge auth tests.
type mockBridgeStore struct {
	store.Store  // embed to satisfy interface (panics on unimplemented calls)
	bridges      map[string]*store.Bridge
	systemConfig map[string]string
}

func newMockBridgeStore() *mockBridgeStore {
	return &mockBridgeStore{
		bridges:      make(map[string]*store.Bridge),
		systemConfig: make(map[string]string),
	}
}

func (m *mockBridgeStore) GetSystemConfig(_ context.Context, key string) (string, error) {
	v, ok := m.systemConfig[key]
	if !ok {
		return "", fmt.Errorf("not found")
	}
	return v, nil
}

func (m *mockBridgeStore) SetSystemConfig(_ context.Context, key, value string) error {
	m.systemConfig[key] = value
	return nil
}

func (m *mockBridgeStore) GetBridge(_ context.Context, _ string, bridgeID string) (*store.Bridge, error) {
	b, ok := m.bridges[bridgeID]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return b, nil
}

func (m *mockBridgeStore) SetBridgeCredentials(_ context.Context, _, bridgeID, username, passwordHash string) error {
	b := m.bridges[bridgeID]
	b.MQTTUsername = username
	b.MQTTPasswordHash = passwordHash
	return nil
}

func (m *mockBridgeStore) SetBridgeCertificate(_ context.Context, _, bridgeID, certPEM string, expiry time.Time) error {
	b := m.bridges[bridgeID]
	b.CertPEM = certPEM
	b.CertExpiry = &expiry
	return nil
}

func (m *mockBridgeStore) ListBridgesWithCredentials(_ context.Context) ([]*store.Bridge, error) {
	var result []*store.Bridge
	for _, b := range m.bridges {
		if b.MQTTUsername != "" {
			result = append(result, b)
		}
	}
	return result, nil
}

func withTenantCtx(ctx context.Context, tid string) context.Context {
	return context.WithValue(ctx, auth.TenantContextKey, tid)
}

func TestGenerateCredentials(t *testing.T) {
	ms := newMockBridgeStore()
	ms.bridges["mule01"] = &store.Bridge{BridgeID: "mule01"}
	ms.systemConfig[mqttPublicURLKey] = "wss://hub.example.com/mqtt"

	handler := NewBridgeAuthHandler(ms, nil)

	r := chi.NewRouter()
	r.Post("/api/bridges/{id}/credentials", handler.GenerateCredentials)

	req := httptest.NewRequest("POST", "/api/bridges/mule01/credentials", nil)
	req = req.WithContext(withTenantCtx(req.Context(), "default"))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp credentialResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.BridgeID != "mule01" {
		t.Errorf("expected bridge_id=mule01, got %s", resp.BridgeID)
	}
	if resp.Username != "mule01" {
		t.Errorf("expected username=mule01, got %s", resp.Username)
	}
	if len(resp.Password) != 64 { // 32 bytes = 64 hex chars
		t.Errorf("expected 64-char password, got %d chars", len(resp.Password))
	}
	if resp.MQTTURL == "" {
		t.Error("expected non-empty MQTT URL")
	}

	// Verify credentials were stored.
	b := ms.bridges["mule01"]
	if b.MQTTUsername != "mule01" {
		t.Errorf("credentials not stored: username=%s", b.MQTTUsername)
	}
	if b.MQTTPasswordHash == "" {
		t.Error("password hash not stored")
	}
}

func TestGenerateCredentials_NotFound(t *testing.T) {
	ms := newMockBridgeStore()
	handler := NewBridgeAuthHandler(ms, nil)

	r := chi.NewRouter()
	r.Post("/api/bridges/{id}/credentials", handler.GenerateCredentials)

	req := httptest.NewRequest("POST", "/api/bridges/nonexistent/credentials", nil)
	req = req.WithContext(withTenantCtx(req.Context(), "default"))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestIssueCertificate(t *testing.T) {
	ms := newMockBridgeStore()
	ms.bridges["mule01"] = &store.Bridge{BridgeID: "mule01"}

	ca, _, _, err := bridge.NewSelfSignedCA("MeshSat Test")
	if err != nil {
		t.Fatalf("setup CA: %v", err)
	}

	handler := NewBridgeAuthHandler(ms, ca)

	r := chi.NewRouter()
	r.Post("/api/bridges/{id}/certificate", handler.IssueCertificate)

	req := httptest.NewRequest("POST", "/api/bridges/mule01/certificate", nil)
	req = req.WithContext(withTenantCtx(req.Context(), "default"))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp certificateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.BridgeID != "mule01" {
		t.Errorf("expected bridge_id=mule01, got %s", resp.BridgeID)
	}
	if resp.CertPEM == "" {
		t.Error("expected non-empty cert_pem")
	}
	if resp.KeyPEM == "" {
		t.Error("expected non-empty key_pem")
	}
	if resp.CaPEM == "" {
		t.Error("expected non-empty ca_pem")
	}
	if resp.Expires == "" {
		t.Error("expected non-empty expires")
	}

	// Verify cert PEM was stored (but NOT the key).
	b := ms.bridges["mule01"]
	if b.CertPEM == "" {
		t.Error("cert not stored in bridge record")
	}
	if b.CertExpiry == nil {
		t.Error("cert expiry not stored")
	}
}

func TestIssueCertificate_NoCA(t *testing.T) {
	ms := newMockBridgeStore()
	ms.bridges["mule01"] = &store.Bridge{BridgeID: "mule01"}

	handler := NewBridgeAuthHandler(ms, nil) // no CA

	r := chi.NewRouter()
	r.Post("/api/bridges/{id}/certificate", handler.IssueCertificate)

	req := httptest.NewRequest("POST", "/api/bridges/mule01/certificate", nil)
	req = req.WithContext(withTenantCtx(req.Context(), "default"))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestIssueCertificate_NotFound(t *testing.T) {
	ms := newMockBridgeStore()
	ca, _, _, _ := bridge.NewSelfSignedCA("Test")
	handler := NewBridgeAuthHandler(ms, ca)

	r := chi.NewRouter()
	r.Post("/api/bridges/{id}/certificate", handler.IssueCertificate)

	req := httptest.NewRequest("POST", "/api/bridges/nonexistent/certificate", nil)
	req = req.WithContext(withTenantCtx(req.Context(), "default"))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// The MQTT public URL is handed to EVERY bridge at onboarding and lives in
// system_config, which has no tenant_id -- one value for the whole platform.
// MESHSAT-1116: the route sat behind nothing but authentication, so any member
// of any tenant, a viewer included, could point all future bridge onboarding at
// a server they control. The role gate is pinned in
// cmd/meshsat-hub/platform_routes_test.go; these cover the handler itself,
// which also decoded the body with json.NewDecoder against critical rule 4.
func TestSetMQTTURLRefusesWhatNoBridgeCouldDial(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantCode   int
	}{
		{"wss is the deployment's own scheme", `{"mqtt_url":"wss://mqtt-hub.meshsat.net/mqtt"}`, http.StatusOK},
		{"tcp for a self-hosted broker", `{"mqtt_url":"tcp://broker.example.com:1883"}`, http.StatusOK},
		{"mqtts", `{"mqtt_url":"mqtts://broker.example.com:8883"}`, http.StatusOK},
		// Everything below reaches no broker. Written into a provisioning
		// bundle it is not a display bug: it is on a kit in a field.
		{"http is not a broker", `{"mqtt_url":"http://evil.example.com"}`, http.StatusBadRequest},
		{"javascript: scheme", `{"mqtt_url":"javascript:alert(1)"}`, http.StatusBadRequest},
		{"file: scheme", `{"mqtt_url":"file:///etc/passwd"}`, http.StatusBadRequest},
		{"no host", `{"mqtt_url":"wss://"}`, http.StatusBadRequest},
		{"not a URL at all", `{"mqtt_url":"just some text"}`, http.StatusBadRequest},
		{"empty", `{"mqtt_url":""}`, http.StatusBadRequest},
		// readJSON, not json.NewDecoder: an unknown field is refused rather
		// than silently dropped, so a typo'd key cannot look like a save.
		{"unknown field is refused", `{"mqtt_url":"wss://a.example.com","admin":true}`, http.StatusBadRequest},
		// ...and a second JSON value cannot smuggle a different one past it.
		{"a second JSON value is refused", `{"mqtt_url":"wss://a.example.com"}{"mqtt_url":"wss://evil.example.com"}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newMockBridgeStore()
			h := NewBridgeAuthHandler(st, nil)
			req := httptest.NewRequest(http.MethodPut, "/api/settings/mqtt-url", strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			h.SetMQTTURL(w, req)
			if w.Code != tc.wantCode {
				t.Errorf("got %d, want %d: %s", w.Code, tc.wantCode, w.Body.String())
			}
			if tc.wantCode != http.StatusOK && st.systemConfig[mqttPublicURLKey] != "" {
				t.Errorf("a refused value was still written: %q", st.systemConfig[mqttPublicURLKey])
			}
		})
	}
}
