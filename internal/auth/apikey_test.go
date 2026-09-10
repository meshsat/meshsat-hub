package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGenerateAPIKey(t *testing.T) {
	plaintext, hash, prefix, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	// Plaintext starts with meshsat_ prefix.
	if !IsAPIKey(plaintext) {
		t.Errorf("plaintext should start with %s, got %q", APIKeyPrefix, plaintext[:16])
	}

	// Prefix is meshsat_ + 8 hex chars.
	if len(prefix) != len(APIKeyPrefix)+8 {
		t.Errorf("prefix length: got %d, want %d", len(prefix), len(APIKeyPrefix)+8)
	}

	// Hash matches the plaintext.
	if HashAPIKey(plaintext) != hash {
		t.Error("hash mismatch")
	}

	// Two generated keys are different.
	plaintext2, _, _, _ := GenerateAPIKey()
	if plaintext == plaintext2 {
		t.Error("two generated keys should be different")
	}
}

func TestIsAPIKey(t *testing.T) {
	tests := []struct {
		token string
		want  bool
	}{
		{"meshsat_abc123", true},
		{"meshsat_", true},
		{"eyJhbGciOiJSUzI1NiJ9.xxx.yyy", false},
		{"some-random-token", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsAPIKey(tt.token); got != tt.want {
			t.Errorf("IsAPIKey(%q) = %v, want %v", tt.token, got, tt.want)
		}
	}
}

func TestHashAPIKey(t *testing.T) {
	hash := HashAPIKey("meshsat_test123")
	if len(hash) != 64 {
		t.Errorf("hash length: got %d, want 64", len(hash))
	}
	// Same input → same hash.
	if HashAPIKey("meshsat_test123") != hash {
		t.Error("hash is not deterministic")
	}
	// Different input → different hash.
	if HashAPIKey("meshsat_other") == hash {
		t.Error("different inputs should produce different hashes")
	}
}

func TestAPIKeyMiddleware_ValidKey(t *testing.T) {
	validator := func(_ context.Context, keyHash string) (*User, string, error) {
		return &User{ID: "apikey:k1", Name: "Test Key", Roles: []string{"operator"}}, "tenant-x", nil
	}

	mw := APIKeyMiddleware(validator)
	handler := mw(okHandler())

	req := httptest.NewRequest("GET", "/api/devices", nil)
	req.Header.Set("Authorization", "Bearer meshsat_abcdef1234567890")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-User-ID"); got != "apikey:k1" {
		t.Errorf("user ID: got %q", got)
	}
}

func TestAPIKeyMiddleware_InvalidKey(t *testing.T) {
	validator := func(_ context.Context, keyHash string) (*User, string, error) {
		return nil, "", fmt.Errorf("not found")
	}

	mw := APIKeyMiddleware(validator)
	handler := mw(okHandler())

	req := httptest.NewRequest("GET", "/api/devices", nil)
	req.Header.Set("Authorization", "Bearer meshsat_badkey")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAPIKeyMiddleware_NonAPIKeyPassesThrough(t *testing.T) {
	validator := func(_ context.Context, keyHash string) (*User, string, error) {
		t.Error("validator should not be called for non-API-key tokens")
		return nil, "", nil
	}

	mw := APIKeyMiddleware(validator)
	handler := mw(okHandler())

	// JWT-like token should pass through.
	req := httptest.NewRequest("GET", "/api/devices", nil)
	req.Header.Set("Authorization", "Bearer eyJhbGciOiJSUzI1NiJ9.xxx.yyy")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200 pass-through, got %d", w.Code)
	}
}

func TestAPIKeyMiddleware_ExpiredKey(t *testing.T) {
	validator := func(_ context.Context, keyHash string) (*User, string, error) {
		return &User{
			ID:        "apikey:k1",
			Roles:     []string{"viewer"},
			ExpiresAt: time.Now().Add(-1 * time.Hour),
		}, "tenant-x", nil
	}

	mw := APIKeyMiddleware(validator)
	handler := mw(okHandler())

	req := httptest.NewRequest("GET", "/api/devices", nil)
	req.Header.Set("Authorization", "Bearer meshsat_expired1234567890")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Errorf("expected 401 for expired key, got %d", w.Code)
	}
}

func TestAPIKeyMiddleware_ExemptPaths(t *testing.T) {
	validator := func(_ context.Context, keyHash string) (*User, string, error) {
		t.Error("validator should not be called for exempt paths")
		return nil, "", nil
	}

	mw := APIKeyMiddleware(validator)
	handler := mw(okHandler())

	for _, path := range []string{"/healthz", "/readyz", "/api/webhook/rockblock"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Errorf("%s: expected 200, got %d", path, w.Code)
		}
	}
}

// The API key middleware and the tenant middleware run as a chain in
// production, and nothing tested them that way: each passed alone while the
// pair answered 403 to every API key request on the live system. The key's
// tenant has to survive from one to the other (MESHSAT-1003).
func TestAPIKeyThenTenantMiddleware_KeepsTheKeysTenant(t *testing.T) {
	validator := func(_ context.Context, _ string) (*User, string, error) {
		// Exactly what the production validator returns: the tenant comes
		// back beside the User, never on it.
		return &User{ID: "apikey:k1", Roles: []string{"owner"}}, "tenant-x", nil
	}

	for _, enforce := range []bool{true, false} {
		var seen string
		handler := APIKeyMiddleware(validator)(
			TenantMiddleware(enforce)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = TenantIDFromContext(r.Context())
				w.WriteHeader(200)
			})))

		req := httptest.NewRequest("GET", "/api/devices", nil)
		req.Header.Set("Authorization", "Bearer meshsat_abcdef1234567890")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != 200 {
			t.Fatalf("enforce=%v: got %d (%s), want 200", enforce, w.Code, w.Body.String())
		}
		if seen != "tenant-x" {
			t.Errorf("enforce=%v: handler saw tenant %q, want tenant-x", enforce, seen)
		}
	}
}

// An API key must never be able to reach across tenants with a header, the way
// a platform admin can.
func TestAPIKeyCannotClaimAnotherTenantByHeader(t *testing.T) {
	validator := func(_ context.Context, _ string) (*User, string, error) {
		return &User{ID: "apikey:k1", Roles: []string{"owner"}}, "tenant-x", nil
	}
	var seen string
	handler := APIKeyMiddleware(validator)(
		TenantMiddleware(true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = TenantIDFromContext(r.Context())
			w.WriteHeader(200)
		})))

	req := httptest.NewRequest("GET", "/api/devices", nil)
	req.Header.Set("Authorization", "Bearer meshsat_abcdef1234567890")
	req.Header.Set("X-Tenant-ID", "tenant-victim")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if seen != "tenant-x" {
		t.Errorf("handler saw tenant %q, want tenant-x -- an API key followed a header", seen)
	}
}
