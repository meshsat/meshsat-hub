package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestID_GeneratesIfMissing(t *testing.T) {
	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := RequestIDFromContext(r.Context())
		if id == "" {
			t.Fatal("expected non-empty request ID in context")
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("expected X-Request-ID response header")
	}
}

func TestRequestID_AcceptsIncoming(t *testing.T) {
	const incoming = "test-req-123"
	var captured string

	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = RequestIDFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Request-ID", incoming)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if captured != incoming {
		t.Fatalf("expected %q, got %q", incoming, captured)
	}
	if rec.Header().Get("X-Request-ID") != incoming {
		t.Fatalf("response header should echo incoming ID")
	}
}

func TestRequestID_RejectsLongID(t *testing.T) {
	long := ""
	for i := 0; i < 65; i++ {
		long += "a"
	}
	var captured string

	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = RequestIDFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Request-ID", long)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if captured == long {
		t.Fatal("should have generated a new ID for overly long input")
	}
	if captured == "" {
		t.Fatal("should still have a request ID")
	}
}

func TestRequestID_RejectsNonASCII(t *testing.T) {
	var captured string

	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = RequestIDFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Request-ID", "id-with-\x00-null")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if captured == "id-with-\x00-null" {
		t.Fatal("should have rejected non-printable ASCII")
	}
}

func TestRequestIDFromContext_EmptyContext(t *testing.T) {
	if id := RequestIDFromContext(context.Background()); id != "" {
		t.Fatalf("expected empty, got %q", id)
	}
}
