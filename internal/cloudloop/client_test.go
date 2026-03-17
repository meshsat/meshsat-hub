package cloudloop

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendMT_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/sbd/mt" {
			t.Errorf("expected /sbd/mt, got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing or wrong auth header: %s", r.Header.Get("Authorization"))
		}

		var req MTRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.IMEI != "300234065123456" {
			t.Errorf("wrong IMEI: %s", req.IMEI)
		}
		decoded, _ := hex.DecodeString(req.Data)
		if string(decoded) != "hello" {
			t.Errorf("wrong payload: %s", string(decoded))
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MTResponse{ID: "mt-123", Status: "queued"})
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-key")
	resp, err := client.SendMT(context.Background(), "300234065123456", []byte("hello"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ID != "mt-123" {
		t.Errorf("expected ID mt-123, got %s", resp.ID)
	}
	if resp.Status != "queued" {
		t.Errorf("expected status queued, got %s", resp.Status)
	}
}

func TestSendMT_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal server error"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-key")
	_, err := client.SendMT(context.Background(), "300234065123456", []byte("hello"))
	if err == nil {
		t.Error("expected error for 500 response")
	}
}

func TestCheckMTStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sbd/mt/mt-123" {
			t.Errorf("expected /sbd/mt/mt-123, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MTResponse{ID: "mt-123", Status: "delivered"})
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-key")
	resp, err := client.CheckMTStatus(context.Background(), "mt-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != "delivered" {
		t.Errorf("expected delivered, got %s", resp.Status)
	}
}

func TestIsReachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-key")
	if !client.IsReachable(context.Background()) {
		t.Error("expected reachable")
	}
}
