package ntfy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPublish_Success(t *testing.T) {
	var gotTitle, gotPriority, gotTags string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/meshsat-sos" {
			t.Errorf("expected /meshsat-sos, got %s", r.URL.Path)
		}
		gotTitle = r.Header.Get("Title")
		gotPriority = r.Header.Get("Priority")
		gotTags = r.Header.Get("Tags")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL)
	err := c.Publish(context.Background(), "meshsat-sos", "SOS Alert", "Device triggered SOS", PriorityUrgent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotTitle != "SOS Alert" {
		t.Errorf("expected title SOS Alert, got %s", gotTitle)
	}
	if gotPriority != PriorityUrgent {
		t.Errorf("expected priority 5, got %s", gotPriority)
	}
	if gotTags != "satellite,warning" {
		t.Errorf("expected tags satellite,warning, got %s", gotTags)
	}
}

func TestNotify_MultipleTopics(t *testing.T) {
	var count int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL)
	err := c.Notify(context.Background(), []string{"topic-a", "topic-b"}, "Alert", "Body")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 publish calls, got %d", count)
	}
}

func TestNotify_EmptyTargets(t *testing.T) {
	c := New("http://unreachable:9999")
	if err := c.Notify(context.Background(), nil, "test", "test"); err != nil {
		t.Errorf("empty targets should not error: %v", err)
	}
}

func TestNotify_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := New(srv.URL)
	err := c.Notify(context.Background(), []string{"test-topic"}, "Alert", "Body")
	if err == nil {
		t.Fatal("expected error on 403")
	}
}

func TestPublish_WithToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL)
	c.SetToken("tk_mytoken")
	_ = c.Publish(context.Background(), "private-topic", "Alert", "Body", PriorityHigh)
	if gotAuth != "Bearer tk_mytoken" {
		t.Errorf("expected Bearer tk_mytoken, got %s", gotAuth)
	}
}

func TestHealthz_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			t.Errorf("expected /v1/health, got %s", r.URL.Path)
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
