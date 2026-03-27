package cloudloop

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/bus"
	"github.com/cubeos-app/meshsat-hub/internal/dedup"
)

// mockBus captures published MQTT messages for testing.
type mockBus struct {
	mu       sync.Mutex
	messages []mockMessage
}

type mockMessage struct {
	topic    string
	payload  []byte
	qos      byte
	retained bool
}

func (b *mockBus) Connect() error                                                { return nil }
func (b *mockBus) Disconnect()                                                   {}
func (b *mockBus) IsConnected() bool                                             { return true }
func (b *mockBus) Subscribe(string, byte, bus.MessageHandler) error              { return nil }
func (b *mockBus) QueueSubscribe(string, byte, string, bus.MessageHandler) error { return nil }

func (b *mockBus) Publish(topic string, qos byte, retained bool, payload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.messages = append(b.messages, mockMessage{topic: topic, payload: payload, qos: qos, retained: retained})
	return nil
}

func (b *mockBus) PublishJSON(topic string, qos byte, retained bool, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return b.Publish(topic, qos, retained, data)
}

func (b *mockBus) getMessages() []mockMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make([]mockMessage, len(b.messages))
	copy(cp, b.messages)
	return cp
}

func newTestLingoMO(imei, text string) LingoMO {
	return LingoMO{
		ID:         "test-id-001",
		ReceivedAt: LingoTimestamp{Year: 2026, Month: 3, Day: 23, Hour: 12, Minute: 0, Second: 0},
		Identity: LingoIdentity{
			AccountID: "acct-001",
			Hardware:  &LingoHardware{ID: "hw-1", Type: "ROCKBLOCK_9704", IMEI: imei, Serial: "rb9704"},
			ThingID:   "thing-001",
		},
		IMT: &LingoIMT{
			CMID:      "cm-001",
			Topic:     "IMT_TOPIC_PURPLE",
			MessageID: "msg-001",
			Size:      len(text),
		},
		Message: base64.StdEncoding.EncodeToString([]byte(text)),
	}
}

func TestWebhookHandler_BasicMO(t *testing.T) {
	bus := &mockBus{}
	h := NewWebhookHandler(bus)
	dd := dedup.NewMemoryDedup(5 * time.Minute)
	defer dd.Close()
	h.SetDedup(dd)

	mo := newTestLingoMO("300258060902280", "Hello from satellite")
	body, _ := json.Marshal(mo)

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/cloudloop", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %q, want %q", resp["status"], "ok")
	}

	// Verify MQTT messages published (mo/raw + mo/decoded).
	msgs := bus.getMessages()
	if len(msgs) < 2 {
		t.Fatalf("expected at least 2 MQTT messages, got %d", len(msgs))
	}

	// Check mo/raw topic.
	if msgs[0].topic != "meshsat/300258060902280/mo/raw" {
		t.Errorf("topic[0] = %q, want %q", msgs[0].topic, "meshsat/300258060902280/mo/raw")
	}

	// Check mo/decoded topic and content.
	if msgs[1].topic != "meshsat/300258060902280/mo/decoded" {
		t.Errorf("topic[1] = %q, want %q", msgs[1].topic, "meshsat/300258060902280/mo/decoded")
	}

	var decoded WebhookMOMessage
	if err := json.Unmarshal(msgs[1].payload, &decoded); err != nil {
		t.Fatalf("decode mo/decoded: %v", err)
	}
	if decoded.IMEI != "300258060902280" {
		t.Errorf("decoded.IMEI = %q, want %q", decoded.IMEI, "300258060902280")
	}
	if decoded.Text != "Hello from satellite" {
		t.Errorf("decoded.Text = %q, want %q", decoded.Text, "Hello from satellite")
	}
	if decoded.Source != "cloudloop_imt" {
		t.Errorf("decoded.Source = %q, want %q", decoded.Source, "cloudloop_imt")
	}
}

func TestWebhookHandler_Dedup(t *testing.T) {
	bus := &mockBus{}
	h := NewWebhookHandler(bus)
	dd := dedup.NewMemoryDedup(5 * time.Minute)
	defer dd.Close()
	h.SetDedup(dd)

	mo := newTestLingoMO("300258060902280", "test dedup")
	body, _ := json.Marshal(mo)

	// First request — should succeed.
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/cloudloop", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	var resp1 map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &resp1)
	if resp1["status"] != "ok" {
		t.Fatalf("first request: status = %q, want %q", resp1["status"], "ok")
	}

	// Second request with same ID — should be deduplicated.
	req2 := httptest.NewRequest(http.MethodPost, "/api/webhook/cloudloop", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)

	var resp2 map[string]string
	_ = json.Unmarshal(rr2.Body.Bytes(), &resp2)
	if resp2["status"] != "duplicate" {
		t.Errorf("second request: status = %q, want %q", resp2["status"], "duplicate")
	}
}

func TestWebhookHandler_PositionExtraction(t *testing.T) {
	bus := &mockBus{}
	h := NewWebhookHandler(bus)

	mo := LingoMO{
		ID:         "pos-test",
		ReceivedAt: LingoTimestamp{Year: 2026, Month: 3, Day: 23, Hour: 12, Minute: 0, Second: 0},
		Identity: LingoIdentity{
			AccountID: "acct",
			Hardware:  &LingoHardware{IMEI: "300258060902280"},
			ThingID:   "t",
		},
		SBD: &LingoSBD{
			IMEI:      "300258060902280",
			MOMSN:     10,
			SessionAt: LingoTimestamp{Year: 2026, Month: 3, Day: 23, Hour: 12, Minute: 0, Second: 0},
			Status:    "OK",
			Location:  &LingoLocation{Latitude: 52.16, Longitude: 4.51, CEP: 10.0},
		},
		Message: base64.StdEncoding.EncodeToString([]byte("position test")),
	}

	status := h.ProcessLingoMO(context.Background(), &mo)
	if status != "ok" {
		t.Fatalf("status = %q, want %q", status, "ok")
	}

	// Should have 3 messages: mo/raw + mo/decoded + position.
	msgs := bus.getMessages()
	if len(msgs) != 3 {
		t.Fatalf("expected 3 MQTT messages, got %d", len(msgs))
	}

	if msgs[2].topic != "meshsat/300258060902280/position" {
		t.Errorf("topic[2] = %q, want position topic", msgs[2].topic)
	}

	var pos WebhookPositionMessage
	if err := json.Unmarshal(msgs[2].payload, &pos); err != nil {
		t.Fatalf("decode position: %v", err)
	}
	if pos.Lat != 52.16 || pos.Lon != 4.51 {
		t.Errorf("position = (%v, %v), want (52.16, 4.51)", pos.Lat, pos.Lon)
	}
	if pos.CEP != 10.0 {
		t.Errorf("CEP = %v, want 10.0", pos.CEP)
	}
}

func TestWebhookHandler_NoIMEI(t *testing.T) {
	bus := &mockBus{}
	h := NewWebhookHandler(bus)

	mo := LingoMO{
		ID:         "no-imei",
		ReceivedAt: LingoTimestamp{Year: 2026, Month: 1, Day: 1},
		Identity:   LingoIdentity{AccountID: "acct", ThingID: "t"},
		Message:    base64.StdEncoding.EncodeToString([]byte("test")),
	}

	status := h.ProcessLingoMO(context.Background(), &mo)
	if status != "error_no_imei" {
		t.Errorf("status = %q, want %q", status, "error_no_imei")
	}
}

func TestWebhookHandler_InvalidJSON(t *testing.T) {
	bus := &mockBus{}
	h := NewWebhookHandler(bus)

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/cloudloop",
		bytes.NewReader([]byte("{invalid json")))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestWebhookHandler_IPAllowlist(t *testing.T) {
	bus := &mockBus{}
	h := NewWebhookHandler(bus)
	h.SetAllowedIPs([]string{"10.0.0.1", "10.0.0.2"})

	mo := newTestLingoMO("300258060902280", "blocked")
	body, _ := json.Marshal(mo)

	// Request from disallowed IP.
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/cloudloop", bytes.NewReader(body))
	req.RemoteAddr = "192.168.1.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("disallowed IP: status = %d, want 403", rr.Code)
	}

	// Request from allowed IP.
	req2 := httptest.NewRequest(http.MethodPost, "/api/webhook/cloudloop", bytes.NewReader(body))
	req2.RemoteAddr = "10.0.0.1:12345"
	req2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()

	h.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusOK {
		t.Errorf("allowed IP: status = %d, want 200", rr2.Code)
	}
}

func newTestLingoMO_SBD(imei, text string) LingoMO {
	return LingoMO{
		ID:         "test-sbd-001",
		ReceivedAt: LingoTimestamp{Year: 2026, Month: 3, Day: 27, Hour: 10, Minute: 0, Second: 0},
		Identity: LingoIdentity{
			AccountID: "acct-001",
			Hardware:  &LingoHardware{ID: "hw-sbd", Type: "ROCKBLOCK_9603", IMEI: imei, Serial: "rb9603"},
			ThingID:   "thing-sbd",
		},
		SBD: &LingoSBD{
			IMEI:      imei,
			MOMSN:     42,
			SessionAt: LingoTimestamp{Year: 2026, Month: 3, Day: 27, Hour: 10, Minute: 0, Second: 0},
			Status:    "OK",
			Location:  &LingoLocation{Latitude: 52.16, Longitude: 4.51, CEP: 10.0},
		},
		Message: base64.StdEncoding.EncodeToString([]byte(text)),
	}
}

func TestWebhookHandler_SBD_MO(t *testing.T) {
	bus := &mockBus{}
	h := NewWebhookHandler(bus)
	dd := dedup.NewMemoryDedup(5 * time.Minute)
	defer dd.Close()
	h.SetDedup(dd)

	mo := newTestLingoMO_SBD("300234063904190", "SBD message from field")
	body, _ := json.Marshal(mo)

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/cloudloop", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %q, want %q", resp["status"], "ok")
	}

	// Verify MQTT messages: mo/raw + mo/decoded + position (SBD has location).
	msgs := bus.getMessages()
	if len(msgs) < 3 {
		t.Fatalf("expected at least 3 MQTT messages (raw+decoded+position), got %d", len(msgs))
	}

	// Check mo/raw topic.
	if msgs[0].topic != "meshsat/300234063904190/mo/raw" {
		t.Errorf("topic[0] = %q, want %q", msgs[0].topic, "meshsat/300234063904190/mo/raw")
	}

	// Check mo/decoded topic and content.
	if msgs[1].topic != "meshsat/300234063904190/mo/decoded" {
		t.Errorf("topic[1] = %q, want %q", msgs[1].topic, "meshsat/300234063904190/mo/decoded")
	}

	var decoded WebhookMOMessage
	if err := json.Unmarshal(msgs[1].payload, &decoded); err != nil {
		t.Fatalf("decode mo/decoded: %v", err)
	}
	if decoded.IMEI != "300234063904190" {
		t.Errorf("decoded.IMEI = %q, want %q", decoded.IMEI, "300234063904190")
	}
	if decoded.Text != "SBD message from field" {
		t.Errorf("decoded.Text = %q, want %q", decoded.Text, "SBD message from field")
	}
	if decoded.Source != "cloudloop_sbd" {
		t.Errorf("decoded.Source = %q, want %q", decoded.Source, "cloudloop_sbd")
	}
	if decoded.MOMSN != 42 {
		t.Errorf("decoded.MOMSN = %d, want 42", decoded.MOMSN)
	}

	// Check position topic (SBD includes Iridium CEP location).
	if msgs[2].topic != "meshsat/300234063904190/position" {
		t.Errorf("topic[2] = %q, want position topic", msgs[2].topic)
	}

	var pos WebhookPositionMessage
	if err := json.Unmarshal(msgs[2].payload, &pos); err != nil {
		t.Fatalf("decode position: %v", err)
	}
	if pos.Lat != 52.16 || pos.Lon != 4.51 {
		t.Errorf("position = (%v, %v), want (52.16, 4.51)", pos.Lat, pos.Lon)
	}
}

func TestWebhookHandler_DualProtocol_SBDandIMT(t *testing.T) {
	bus := &mockBus{}
	h := NewWebhookHandler(bus)
	dd := dedup.NewMemoryDedup(5 * time.Minute)
	defer dd.Close()
	h.SetDedup(dd)

	// Send SBD MO.
	sbdMO := newTestLingoMO_SBD("300234063904190", "SBD path")
	sbdBody, _ := json.Marshal(sbdMO)
	req1 := httptest.NewRequest(http.MethodPost, "/api/webhook/cloudloop", bytes.NewReader(sbdBody))
	req1.Header.Set("Content-Type", "application/json")
	rr1 := httptest.NewRecorder()
	h.ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusOK {
		t.Fatalf("SBD: status = %d, want 200", rr1.Code)
	}

	// Send IMT MO (different IMEI to avoid dedup).
	imtMO := newTestLingoMO("300258060902280", "IMT path")
	imtBody, _ := json.Marshal(imtMO)
	req2 := httptest.NewRequest(http.MethodPost, "/api/webhook/cloudloop", bytes.NewReader(imtBody))
	req2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("IMT: status = %d, want 200", rr2.Code)
	}

	// Verify both protocols published correctly.
	msgs := bus.getMessages()
	sources := make(map[string]bool)
	for _, m := range msgs {
		var decoded WebhookMOMessage
		if json.Unmarshal(m.payload, &decoded) == nil && decoded.Source != "" {
			sources[decoded.Source] = true
		}
	}

	if !sources["cloudloop_sbd"] {
		t.Error("missing cloudloop_sbd source in published messages")
	}
	if !sources["cloudloop_imt"] {
		t.Error("missing cloudloop_imt source in published messages")
	}
}

func TestWebhookHandler_MethodNotAllowed(t *testing.T) {
	bus := &mockBus{}
	h := NewWebhookHandler(bus)

	req := httptest.NewRequest(http.MethodGet, "/api/webhook/cloudloop", nil)
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}
