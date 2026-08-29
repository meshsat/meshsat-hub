package sms

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/bus"
)

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
func (m *mockBus) Publish(topic string, _ byte, _ bool, payload []byte) error {
	m.published = append(m.published, mockMsg{topic: topic, data: payload})
	return nil
}
func (m *mockBus) PublishJSON(topic string, qos byte, retained bool, v any) error {
	data, _ := json.Marshal(v)
	return m.Publish(topic, qos, retained, data)
}

func TestWebhook_ValidInbound(t *testing.T) {
	mb := &mockBus{}
	h := NewWebhookHandler(mb, "")

	form := url.Values{
		"From":       {"+31612345678"},
		"To":         {"+31698765432"},
		"Body":       {"Hello from phone"},
		"MessageSid": {"SM999"},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/sms", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if len(mb.published) != 1 {
		t.Fatalf("expected 1 MQTT publish, got %d", len(mb.published))
	}
	if mb.published[0].topic != "meshsat/hub/sms/inbound" {
		t.Errorf("topic = %s, want meshsat/hub/sms/inbound", mb.published[0].topic)
	}

	var msg InboundSMS
	_ = json.Unmarshal(mb.published[0].data, &msg)
	if msg.From != "+31612345678" {
		t.Errorf("from = %s, want +31612345678", msg.From)
	}
	if msg.Body != "Hello from phone" {
		t.Errorf("body = %s, want 'Hello from phone'", msg.Body)
	}
}

func TestWebhook_MissingFrom(t *testing.T) {
	h := NewWebhookHandler(&mockBus{}, "")

	form := url.Values{"Body": {"test"}}
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/sms", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestWebhook_MissingBody(t *testing.T) {
	h := NewWebhookHandler(&mockBus{}, "")

	form := url.Values{"From": {"+1"}}
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/sms", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestWebhook_MethodNotAllowed(t *testing.T) {
	h := NewWebhookHandler(&mockBus{}, "")

	req := httptest.NewRequest(http.MethodGet, "/api/webhook/sms", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestWebhook_MissingSignature(t *testing.T) {
	h := NewWebhookHandler(&mockBus{}, "secret123")

	form := url.Values{"From": {"+1"}, "Body": {"test"}}
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/sms", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}
