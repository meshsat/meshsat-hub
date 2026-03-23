package cloudloop

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendSBD_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/Data/DoSendSbdMessage" {
			t.Errorf("expected /Data/DoSendSbdMessage, got %s", r.URL.Path)
		}

		token := r.URL.Query().Get("token")
		if token != "test-key" {
			t.Errorf("wrong token: %s", token)
		}

		thing := r.URL.Query().Get("thing")
		if thing != "AbCdEfGh12345" {
			t.Errorf("wrong thing: %s", thing)
		}

		msg := r.URL.Query().Get("message")
		decoded, _ := hex.DecodeString(msg)
		if string(decoded) != "hello" {
			t.Errorf("wrong payload: %s (hex: %s)", string(decoded), msg)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MTResponse{ID: "sbd-123", Status: "queued"})
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-key")
	resp, err := client.SendSBD(context.Background(), "AbCdEfGh12345", []byte("hello"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ID != "sbd-123" {
		t.Errorf("expected ID sbd-123, got %s", resp.ID)
	}
	if resp.Status != "queued" {
		t.Errorf("expected status queued, got %s", resp.Status)
	}
}

func TestSendIMT_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/Data/DoSendImtMessage" {
			t.Errorf("expected /Data/DoSendImtMessage, got %s", r.URL.Path)
		}

		token := r.URL.Query().Get("token")
		if token != "test-key" {
			t.Errorf("wrong token: %s", token)
		}

		thing := r.URL.Query().Get("thing")
		if thing != "XyZ9704Thing" {
			t.Errorf("wrong thing: %s", thing)
		}

		topic := r.URL.Query().Get("topic")
		if topic != IMTTopicPurple {
			t.Errorf("wrong topic: %s", topic)
		}

		ringStyle := r.URL.Query().Get("ringStyle")
		if ringStyle != RingStyleUrgent {
			t.Errorf("wrong ringStyle: %s", ringStyle)
		}

		msg := r.URL.Query().Get("message")
		decoded, err := base64.StdEncoding.DecodeString(msg)
		if err != nil {
			t.Fatalf("base64 decode: %v", err)
		}
		if string(decoded) != "IMT test payload with more data" {
			t.Errorf("wrong payload: %s", string(decoded))
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MTResponse{ID: "imt-456", Status: "queued"})
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-key")
	resp, err := client.SendIMT(context.Background(), "XyZ9704Thing", []byte("IMT test payload with more data"), IMTTopicPurple, RingStyleUrgent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ID != "imt-456" {
		t.Errorf("expected ID imt-456, got %s", resp.ID)
	}
}

func TestSendIMT_DefaultTopicAndRing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		topic := r.URL.Query().Get("topic")
		if topic != IMTTopicRaw {
			t.Errorf("expected default topic %s, got %s", IMTTopicRaw, topic)
		}

		ringStyle := r.URL.Query().Get("ringStyle")
		if ringStyle != RingStyleNormal {
			t.Errorf("expected default ring %s, got %s", RingStyleNormal, ringStyle)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MTResponse{ID: "imt-def", Status: "queued"})
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-key")
	_, err := client.SendIMT(context.Background(), "thing1", []byte("test"), "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSendIMT_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid thing ID"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-key")
	_, err := client.SendIMT(context.Background(), "bad-thing", []byte("test"), "", "")
	if err == nil {
		t.Error("expected error for 400 response")
	}
}

func TestGetDeliveryStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/Data/GetMtDeliveryStatus" {
			t.Errorf("expected /Data/GetMtDeliveryStatus, got %s", r.URL.Path)
		}

		msgID := r.URL.Query().Get("message")
		if msgID != "imt-456" {
			t.Errorf("wrong message ID: %s", msgID)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MTResponse{
			ID:              "imt-456",
			Status:          "delivered",
			MTMessageStatus: 1,
		})
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-key")
	resp, err := client.GetDeliveryStatus(context.Background(), "imt-456")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != "delivered" {
		t.Errorf("expected delivered, got %s", resp.Status)
	}
	if resp.MTMessageStatus != 1 {
		t.Errorf("expected mtMessageStatus 1, got %d", resp.MTMessageStatus)
	}
}

// Legacy API tests (backwards compatibility)

func TestSendMT_Legacy_Success(t *testing.T) {
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

		var req struct {
			IMEI string `json:"imei"`
			Data string `json:"data"`
		}
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
}

func TestSendMT_Legacy_APIError(t *testing.T) {
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

func TestCheckMTStatus_Legacy(t *testing.T) {
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

func TestIMTTopicConstants(t *testing.T) {
	topics := []string{IMTTopicPurple, IMTTopicPink, IMTTopicRed, IMTTopicOrange, IMTTopicYellow, IMTTopicRaw, IMTTopicRockRemote}
	for _, topic := range topics {
		if topic == "" {
			t.Error("empty topic constant")
		}
	}
}
