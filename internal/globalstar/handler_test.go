package globalstar

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/bus"
	"github.com/cubeos-app/meshsat-hub/internal/dedup"
)

// mockBus implements bus.MessageBus for testing.
type mockBus struct {
	published []mockMsg
}
type mockMsg struct {
	topic string
	data  []byte
}

func (m *mockBus) Connect() error                                                { return nil }
func (m *mockBus) Disconnect()                                                   {}
func (m *mockBus) IsConnected() bool                                             { return true }
func (m *mockBus) Subscribe(string, byte, bus.MessageHandler) error              { return nil }
func (m *mockBus) QueueSubscribe(string, byte, string, bus.MessageHandler) error { return nil }
func (m *mockBus) Publish(topic string, qos byte, retained bool, payload []byte) error {
	m.published = append(m.published, mockMsg{topic: topic, data: payload})
	return nil
}
func (m *mockBus) PublishJSON(topic string, qos byte, retained bool, v any) error {
	data, _ := json.Marshal(v)
	m.published = append(m.published, mockMsg{topic: topic, data: data})
	return nil
}

func newTestPayload(deviceID, msgID, text string) MOPayload {
	return MOPayload{
		DeviceID:   deviceID,
		MessageID:  msgID,
		Data:       base64.StdEncoding.EncodeToString([]byte(text)),
		ReceivedAt: "2026-03-20T10:00:00Z",
		Latitude:   52.1621,
		Longitude:  4.5094,
	}
}

func postWebhook(h *Handler, payload MOPayload) *httptest.ResponseRecorder {
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", "/api/webhook/globalstar", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func postWebhookSigned(h *Handler, payload MOPayload, secret string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := fmt.Sprintf("%x", mac.Sum(nil))

	req := httptest.NewRequest("POST", "/api/webhook/globalstar", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature", sig)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestServeHTTP_ValidPayload(t *testing.T) {
	mb := &mockBus{}
	h := NewHandler(mb, "")
	payload := newTestPayload("dev-gs-001", "msg-abc", "Hello from Globalstar")

	w := postWebhook(h, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %s", resp["status"])
	}

	// Should publish to mo/raw and mo/decoded (and position since lat/lon set).
	if len(mb.published) < 2 {
		t.Fatalf("expected at least 2 MQTT publishes, got %d", len(mb.published))
	}
	if mb.published[0].topic != "meshsat/dev-gs-001/mo/raw" {
		t.Errorf("expected mo/raw topic, got %s", mb.published[0].topic)
	}
	if mb.published[1].topic != "meshsat/dev-gs-001/mo/decoded" {
		t.Errorf("expected mo/decoded topic, got %s", mb.published[1].topic)
	}
}

func TestServeHTTP_MissingDeviceID(t *testing.T) {
	h := NewHandler(&mockBus{}, "")
	payload := MOPayload{
		MessageID: "msg-abc",
		Data:      base64.StdEncoding.EncodeToString([]byte("test")),
	}

	w := postWebhook(h, payload)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestServeHTTP_InvalidBase64(t *testing.T) {
	h := NewHandler(&mockBus{}, "")
	payload := MOPayload{
		DeviceID:  "dev-gs-001",
		MessageID: "msg-abc",
		Data:      "not-valid-base64!!!",
	}

	w := postWebhook(h, payload)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestServeHTTP_MethodNotAllowed(t *testing.T) {
	h := NewHandler(&mockBus{}, "")
	req := httptest.NewRequest("GET", "/api/webhook/globalstar", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestServeHTTP_Dedup(t *testing.T) {
	mb := &mockBus{}
	h := NewHandler(mb, "")
	h.SetDedup(dedup.NewMemoryDedup(1 * time.Hour))
	payload := newTestPayload("dev-gs-001", "msg-abc", "Hello")

	// First request: accepted.
	w := postWebhook(h, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", w.Code)
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "ok" {
		t.Fatalf("first request: expected ok, got %s", resp["status"])
	}

	// Second request: duplicate.
	w = postWebhook(h, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("duplicate request: expected 200, got %d", w.Code)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "duplicate" {
		t.Fatalf("duplicate request: expected duplicate, got %s", resp["status"])
	}
}

func TestServeHTTP_SignatureRequired(t *testing.T) {
	secret := "test-secret-key"
	h := NewHandler(&mockBus{}, secret)
	payload := newTestPayload("dev-gs-001", "msg-abc", "Hello")

	// No signature — rejected.
	w := postWebhook(h, payload)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without signature, got %d: %s", w.Code, w.Body.String())
	}
}

func TestServeHTTP_ValidSignature(t *testing.T) {
	secret := "test-secret-key"
	mb := &mockBus{}
	h := NewHandler(mb, secret)
	payload := newTestPayload("dev-gs-001", "msg-abc", "Hello signed")

	w := postWebhookSigned(h, payload, secret)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid signature, got %d: %s", w.Code, w.Body.String())
	}
}

func TestServeHTTP_InvalidSignature(t *testing.T) {
	h := NewHandler(&mockBus{}, "correct-secret")
	payload := newTestPayload("dev-gs-001", "msg-abc", "Hello")

	w := postWebhookSigned(h, payload, "wrong-secret")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong signature, got %d", w.Code)
	}
}

func TestServeHTTP_PositionPublished(t *testing.T) {
	mb := &mockBus{}
	h := NewHandler(mb, "")
	payload := newTestPayload("dev-gs-001", "msg-abc", "pos test")
	payload.Latitude = 52.16
	payload.Longitude = 4.51

	w := postWebhook(h, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Should have 3 publishes: raw, decoded, position.
	if len(mb.published) != 3 {
		t.Fatalf("expected 3 publishes, got %d", len(mb.published))
	}
	if mb.published[2].topic != "meshsat/dev-gs-001/position" {
		t.Errorf("expected position topic, got %s", mb.published[2].topic)
	}
}

func TestServeHTTP_NoPositionWhenZero(t *testing.T) {
	mb := &mockBus{}
	h := NewHandler(mb, "")
	payload := newTestPayload("dev-gs-001", "msg-abc", "no pos")
	payload.Latitude = 0
	payload.Longitude = 0

	w := postWebhook(h, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Should have 2 publishes: raw, decoded (no position).
	if len(mb.published) != 2 {
		t.Fatalf("expected 2 publishes (no position), got %d", len(mb.published))
	}
}

func TestServeHTTP_DecodedMessageChannel(t *testing.T) {
	mb := &mockBus{}
	h := NewHandler(mb, "")
	payload := newTestPayload("dev-gs-001", "msg-abc", "channel test")

	_ = postWebhook(h, payload)

	// Check the decoded message has channel "globalstar".
	var decoded MOMessage
	_ = json.Unmarshal(mb.published[1].data, &decoded)
	if decoded.Channel != "globalstar" {
		t.Errorf("expected channel globalstar, got %s", decoded.Channel)
	}
	if decoded.DeviceID != "dev-gs-001" {
		t.Errorf("expected device_id dev-gs-001, got %s", decoded.DeviceID)
	}
}
