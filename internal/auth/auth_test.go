package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func makeJWT(claims map[string]interface{}) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, _ := json.Marshal(claims)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	sig := base64.RawURLEncoding.EncodeToString([]byte("fake-signature"))
	return fmt.Sprintf("%s.%s.%s", header, payloadB64, sig)
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := FromContext(r.Context())
		if user != nil {
			w.Header().Set("X-User-ID", user.ID)
		}
		w.WriteHeader(200)
	})
}

func TestNoopMiddleware(t *testing.T) {
	mw := Middleware(Config{Mode: "none"})
	handler := mw(okHandler())

	req := httptest.NewRequest("GET", "/api/devices", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Header().Get("X-User-ID") != "anonymous" {
		t.Errorf("user: %q", w.Header().Get("X-User-ID"))
	}
}

func TestTokenMiddleware_ValidToken(t *testing.T) {
	mw := Middleware(Config{Mode: "token", Token: "secret123"})
	handler := mw(okHandler())

	req := httptest.NewRequest("GET", "/api/devices", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestTokenMiddleware_InvalidToken(t *testing.T) {
	mw := Middleware(Config{Mode: "token", Token: "secret123"})
	handler := mw(okHandler())

	req := httptest.NewRequest("GET", "/api/devices", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestTokenMiddleware_MissingHeader(t *testing.T) {
	mw := Middleware(Config{Mode: "token", Token: "secret123"})
	handler := mw(okHandler())

	req := httptest.NewRequest("GET", "/api/devices", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestTokenMiddleware_ExemptPaths(t *testing.T) {
	mw := Middleware(Config{Mode: "token", Token: "secret123"})
	handler := mw(okHandler())

	for _, path := range []string{"/healthz", "/readyz", "/", "/index.html", "/api/webhook/rockblock"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Errorf("%s: expected 200, got %d", path, w.Code)
		}
	}
}

func TestJWTMiddleware_ValidToken(t *testing.T) {
	mw := Middleware(Config{
		Mode:          "oidc",
		OIDCIssuerURL: "https://auth.example.com",
		OIDCAudience:  "meshsat-hub",
	})
	handler := mw(okHandler())

	token := makeJWT(map[string]interface{}{
		"sub":       "user-123",
		"email":     "test@example.com",
		"name":      "Test User",
		"iss":       "https://auth.example.com",
		"aud":       "meshsat-hub",
		"exp":       time.Now().Add(1 * time.Hour).Unix(),
		"roles":     []string{"admin"},
		"tenant_id": "tenant-1",
	})

	req := httptest.NewRequest("GET", "/api/devices", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-User-ID") != "user-123" {
		t.Errorf("user: %q", w.Header().Get("X-User-ID"))
	}
}

func TestJWTMiddleware_ExpiredToken(t *testing.T) {
	mw := Middleware(Config{
		Mode:          "oidc",
		OIDCIssuerURL: "https://auth.example.com",
	})
	handler := mw(okHandler())

	token := makeJWT(map[string]interface{}{
		"sub": "user-123",
		"iss": "https://auth.example.com",
		"exp": time.Now().Add(-1 * time.Hour).Unix(), // expired
	})

	req := httptest.NewRequest("GET", "/api/devices", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Errorf("expected 401 for expired token, got %d", w.Code)
	}
}

func TestJWTMiddleware_WrongIssuer(t *testing.T) {
	mw := Middleware(Config{
		Mode:          "oidc",
		OIDCIssuerURL: "https://auth.example.com",
	})
	handler := mw(okHandler())

	token := makeJWT(map[string]interface{}{
		"sub": "user-123",
		"iss": "https://evil.example.com",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})

	req := httptest.NewRequest("GET", "/api/devices", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Errorf("expected 401 for wrong issuer, got %d", w.Code)
	}
}

func TestFromContext_Empty(t *testing.T) {
	user := FromContext(context.Background())
	if user != nil {
		t.Error("expected nil user from empty context")
	}
}

func TestUser_HasRole(t *testing.T) {
	u := &User{Roles: []string{"admin", "viewer"}}
	if !u.HasRole("admin") {
		t.Error("should have admin role")
	}
	if u.HasRole("superadmin") {
		t.Error("should not have superadmin role")
	}
}
