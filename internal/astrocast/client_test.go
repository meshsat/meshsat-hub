package astrocast

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendMessage_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/commands" {
			t.Errorf("expected /v1/commands, got %s", r.URL.Path)
		}
		if r.Header.Get("X-Api-Key") != "test-key" {
			t.Errorf("missing or wrong API key header: %s", r.Header.Get("X-Api-Key"))
		}

		var req SendRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.DeviceGUID != "device-abc-123" {
			t.Errorf("wrong device GUID: %s", req.DeviceGUID)
		}
		decoded, err := base64.StdEncoding.DecodeString(req.Data)
		if err != nil {
			t.Fatalf("decode base64: %v", err)
		}
		if string(decoded) != "hello astrocast" {
			t.Errorf("wrong payload: %s", string(decoded))
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(SendResponse{ID: "cmd-456", Status: "queued"})
	}))
	defer server.Close()

	client := NewClient(server.URL+"/v1", "test-key")
	resp, err := client.SendMessage(context.Background(), "device-abc-123", []byte("hello astrocast"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ID != "cmd-456" {
		t.Errorf("expected ID cmd-456, got %s", resp.ID)
	}
	if resp.Status != "queued" {
		t.Errorf("expected status queued, got %s", resp.Status)
	}
}

func TestSendMessage_PayloadTooLarge(t *testing.T) {
	client := NewClient(DefaultAPIURL, "test-key")
	payload := make([]byte, MaxPayloadBytes+1)
	_, err := client.SendMessage(context.Background(), "device-1", payload)
	if err == nil {
		t.Error("expected error for oversized payload")
	}
}

func TestSendMessage_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"invalid API key"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL+"/v1", "bad-key")
	_, err := client.SendMessage(context.Background(), "device-1", []byte("hello"))
	if err == nil {
		t.Error("expected error for 403 response")
	}
}

func TestCheckMessageStatus_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/v1/commands/cmd-456" {
			t.Errorf("expected /v1/commands/cmd-456, got %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MessageStatus{ID: "cmd-456", Status: "delivered"})
	}))
	defer server.Close()

	client := NewClient(server.URL+"/v1", "test-key")
	status, err := client.CheckMessageStatus(context.Background(), "cmd-456")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.Status != "delivered" {
		t.Errorf("expected delivered, got %s", status.Status)
	}
}

func TestCheckMessageStatus_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"command not found"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL+"/v1", "test-key")
	_, err := client.CheckMessageStatus(context.Background(), "nonexistent")
	if err == nil {
		t.Error("expected error for 404 response")
	}
}

func TestGetDeviceStatus_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/devices/device-abc-123" {
			t.Errorf("expected /v1/devices/device-abc-123, got %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(DeviceStatus{
			DeviceGUID: "device-abc-123",
			Name:       "Field Unit 1",
			Online:     true,
			LastSeen:   "2026-03-18T10:00:00Z",
		})
	}))
	defer server.Close()

	client := NewClient(server.URL+"/v1", "test-key")
	status, err := client.GetDeviceStatus(context.Background(), "device-abc-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !status.Online {
		t.Error("expected device to be online")
	}
	if status.Name != "Field Unit 1" {
		t.Errorf("expected name 'Field Unit 1', got %s", status.Name)
	}
}

func TestIsReachable_Healthy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(server.URL+"/v1", "test-key")
	if !client.IsReachable(context.Background()) {
		t.Error("expected reachable")
	}
}

func TestIsReachable_Down(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewClient(server.URL+"/v1", "test-key")
	if client.IsReachable(context.Background()) {
		t.Error("expected unreachable for 500")
	}
}

func TestNewClient_DefaultURL(t *testing.T) {
	client := NewClient("", "test-key")
	if client.apiURL != DefaultAPIURL {
		t.Errorf("expected default URL %s, got %s", DefaultAPIURL, client.apiURL)
	}
}
