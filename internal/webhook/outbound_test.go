package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestFire_DeliversToMatchingWebhook(t *testing.T) {
	var received []byte
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := NewDispatcher(nil)
	d.AllowLoopbackTargetsForTest() // httptest listens on 127.0.0.1
	d.AddWebhook(WebhookConfig{
		TenantID: tenantA,
		ID:       "test-1",
		URL:      srv.URL,
		Events:   []EventType{EventMO},
		Enabled:  true,
	})

	data, _ := json.Marshal(map[string]string{"text": "hello"})
	d.Fire(tenantA, EventMO, "device-1", data)

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if received == nil {
		t.Fatal("webhook not received")
	}

	var payload WebhookPayload
	if err := json.Unmarshal(received, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Event != EventMO {
		t.Errorf("event: got %q, want mo", payload.Event)
	}
	if payload.DeviceID != "device-1" {
		t.Errorf("device_id: got %q", payload.DeviceID)
	}
}

func TestFire_SkipsNonMatchingEvents(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := NewDispatcher(nil)
	d.AllowLoopbackTargetsForTest() // httptest listens on 127.0.0.1
	d.AddWebhook(WebhookConfig{
		TenantID: tenantA,
		ID:       "sos-only",
		URL:      srv.URL,
		Events:   []EventType{EventSOS}, // only SOS
		Enabled:  true,
	})

	// Fire MO event — should not match
	d.Fire(tenantA, EventMO, "device-1", json.RawMessage(`{}`))
	time.Sleep(200 * time.Millisecond)

	if called {
		t.Error("webhook should not be called for non-matching event")
	}
}

func TestFire_SkipsDisabledWebhook(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := NewDispatcher(nil)
	d.AllowLoopbackTargetsForTest() // httptest listens on 127.0.0.1
	d.AddWebhook(WebhookConfig{
		TenantID: tenantA,
		ID:       "disabled",
		URL:      srv.URL,
		Events:   []EventType{EventMO},
		Enabled:  false, // disabled
	})

	d.Fire(tenantA, EventMO, "device-1", json.RawMessage(`{}`))
	time.Sleep(200 * time.Millisecond)

	if called {
		t.Error("disabled webhook should not be called")
	}
}

func TestFire_HMACSigning(t *testing.T) {
	secret := "test-secret-key"
	var signature string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signature = r.Header.Get("X-Hub-Signature-256")
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := NewDispatcher(nil)
	d.AllowLoopbackTargetsForTest() // httptest listens on 127.0.0.1
	d.AddWebhook(WebhookConfig{
		TenantID: tenantA,
		ID:       "signed",
		URL:      srv.URL,
		Secret:   secret,
		Events:   []EventType{EventSOS},
		Enabled:  true,
	})

	d.Fire(tenantA, EventSOS, "device-1", json.RawMessage(`{"triggered":true}`))
	time.Sleep(200 * time.Millisecond)

	if signature == "" {
		t.Fatal("missing X-Hub-Signature-256 header")
	}
	if len(signature) < 8 || signature[:7] != "sha256=" {
		t.Fatalf("bad signature format: %q", signature)
	}

	// Verify HMAC
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if signature != expected {
		t.Errorf("signature mismatch:\n  got:  %s\n  want: %s", signature, expected)
	}
}

func TestFire_RetriesOnFailure(t *testing.T) {
	var attempts int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		a := attempts
		mu.Unlock()
		if a <= 2 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := NewDispatcher(nil)
	d.AllowLoopbackTargetsForTest() // httptest listens on 127.0.0.1
	d.AddWebhook(WebhookConfig{
		TenantID:   tenantA,
		ID:         "retry-test",
		URL:        srv.URL,
		Events:     []EventType{EventMO},
		Enabled:    true,
		MaxRetries: 3,
		TimeoutSec: 5,
	})

	d.Fire(tenantA, EventMO, "device-1", json.RawMessage(`{}`))
	time.Sleep(5 * time.Second) // allow retries

	mu.Lock()
	a := attempts
	mu.Unlock()
	if a < 3 {
		t.Errorf("expected at least 3 attempts, got %d", a)
	}
}

func TestAddRemoveWebhook(t *testing.T) {
	d := NewDispatcher(nil)
	d.AllowLoopbackTargetsForTest() // httptest listens on 127.0.0.1
	d.AddWebhook(WebhookConfig{TenantID: tenantA, ID: "a", URL: "http://a.com", Enabled: true})
	d.AddWebhook(WebhookConfig{TenantID: tenantA, ID: "b", URL: "http://b.com", Enabled: true})

	if len(d.ListWebhooks(tenantA)) != 2 {
		t.Fatalf("expected 2 webhooks, got %d", len(d.ListWebhooks(tenantA)))
	}

	d.RemoveWebhook(tenantA, "a")
	if len(d.ListWebhooks(tenantA)) != 1 {
		t.Fatalf("expected 1 webhook after remove, got %d", len(d.ListWebhooks(tenantA)))
	}
	if d.ListWebhooks(tenantA)[0].ID != "b" {
		t.Errorf("remaining webhook: got %q, want b", d.ListWebhooks(tenantA)[0].ID)
	}
}

func TestListWebhooks_RedactsSecret(t *testing.T) {
	d := NewDispatcher(nil)
	d.AllowLoopbackTargetsForTest() // httptest listens on 127.0.0.1
	d.AddWebhook(WebhookConfig{TenantID: tenantA, ID: "secret", URL: "http://a.com", Secret: "my-secret", Enabled: true})

	list := d.ListWebhooks(tenantA)
	if list[0].Secret != "****" {
		t.Errorf("secret not redacted: %q", list[0].Secret)
	}
}

func TestRecentLogs(t *testing.T) {
	d := NewDispatcher(nil)
	d.AllowLoopbackTargetsForTest() // httptest listens on 127.0.0.1
	d.recordLog(DeliveryLog{TenantID: tenantA, WebhookID: "wh-1", Event: "mo", DeviceID: "device-1", StatusCode: 200, Error: "", Attempt: 0})
	d.recordLog(DeliveryLog{TenantID: tenantA, WebhookID: "wh-1", Event: "sos", DeviceID: "device-2", StatusCode: 500, Error: "server error", Attempt: 1})

	logs := d.RecentLogs(tenantA, 10)
	if len(logs) != 2 {
		t.Fatalf("expected 2 logs, got %d", len(logs))
	}
	if logs[0].StatusCode != 200 {
		t.Errorf("log[0] status: %d", logs[0].StatusCode)
	}
	if logs[1].Error != "server error" {
		t.Errorf("log[1] error: %q", logs[1].Error)
	}
}
