package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/bridge"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/go-chi/chi/v5"
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
