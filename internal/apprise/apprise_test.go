package apprise

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNotify_Success(t *testing.T) {
	var received notifyRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/notify/" {
			t.Errorf("expected /notify/, got %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL)
	err := c.Notify(context.Background(),
		[]string{"slack://token/channel", "mailto://user:pass@gmail.com"},
		"SOS Alert", "Device 123 triggered SOS")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if received.URLs != "slack://token/channel,mailto://user:pass@gmail.com" {
		t.Errorf("unexpected URLs: %s", received.URLs)
	}
	if received.Title != "SOS Alert" {
		t.Errorf("unexpected title: %s", received.Title)
	}
	if received.Body != "Device 123 triggered SOS" {
		t.Errorf("unexpected body: %s", received.Body)
	}
	if received.Type != "warning" {
		t.Errorf("unexpected type: %s", received.Type)
	}
}

func TestNotify_EmptyTargets(t *testing.T) {
	c := New("http://unreachable:9999")
	err := c.Notify(context.Background(), nil, "test", "test")
	if err != nil {
		t.Errorf("empty targets should not error: %v", err)
	}
}

func TestNotify_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	err := c.Notify(context.Background(), []string{"slack://x"}, "test", "body")
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
}

func TestNotify_ConnectionRefused(t *testing.T) {
	c := New("http://127.0.0.1:1") // unlikely to be listening
	err := c.Notify(context.Background(), []string{"slack://x"}, "test", "body")
	if err == nil {
		t.Fatal("expected error on connection refused")
	}
}

func TestNotifyStateful_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/notify/mykey/" {
			t.Errorf("expected /notify/mykey/, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL)
	err := c.NotifyStateful(context.Background(), "mykey", "Alert", "Body", "failure")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHealthz_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status/" {
			t.Errorf("expected /status/, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL)
	if err := c.Healthz(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHealthz_Down(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(srv.URL)
	if err := c.Healthz(context.Background()); err == nil {
		t.Fatal("expected error on 503")
	}
}
