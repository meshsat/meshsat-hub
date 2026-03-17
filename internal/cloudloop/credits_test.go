package cloudloop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetCreditBalance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/account/balance" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"balance":  42,
			"currency": "credits",
		})
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-key")
	balance, err := client.GetCreditBalance(context.Background())
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if balance.Balance != 42 {
		t.Errorf("balance: got %d, want 42", balance.Balance)
	}
	if balance.Timestamp == "" {
		t.Error("timestamp should be set")
	}
}

func TestGetCreditBalance_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-key")
	_, err := client.GetCreditBalance(context.Background())
	if err == nil {
		t.Error("expected error on 500 response")
	}
}
