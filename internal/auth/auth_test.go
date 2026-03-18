package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// testKeyPair holds an RSA key pair and its kid for testing.
type testKeyPair struct {
	kid        string
	privateKey *rsa.PrivateKey
	publicKey  *rsa.PublicKey
}

func generateTestKey(kid string) *testKeyPair {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return &testKeyPair{kid: kid, privateKey: priv, publicKey: &priv.PublicKey}
}

// signJWT creates a signed JWT with the given claims and key.
func signJWT(kp *testKeyPair, claims jwt.MapClaims) string {
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kp.kid
	signed, err := token.SignedString(kp.privateKey)
	if err != nil {
		panic(err)
	}
	return signed
}

// startMockOIDCServer starts a test HTTP server that serves OIDC discovery and JWKS endpoints.
func startMockOIDCServer(keys ...*testKeyPair) *httptest.Server {
	mux := http.NewServeMux()

	var jwksKeys []map[string]string
	for _, kp := range keys {
		jwksKeys = append(jwksKeys, map[string]string{
			"kty": "RSA",
			"use": "sig",
			"kid": kp.kid,
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(kp.publicKey.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(kp.publicKey.E)).Bytes()),
		})
	}

	var srv *httptest.Server

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]string{
			"issuer":   srv.URL,
			"jwks_uri": srv.URL + "/jwks",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})

	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{"keys": jwksKeys}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	srv = httptest.NewServer(mux)
	return srv
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := FromContext(r.Context())
		if user != nil {
			w.Header().Set("X-User-ID", user.ID)
			w.Header().Set("X-User-Email", user.Email)
			w.Header().Set("X-User-Tenant", user.TenantID)
		}
		w.WriteHeader(200)
	})
}

// --- Noop mode tests ---

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

// --- Token mode tests ---

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

// --- OIDC JWT tests (with real RSA signature verification) ---

func TestJWTMiddleware_ValidToken(t *testing.T) {
	kp := generateTestKey("key-1")
	srv := startMockOIDCServer(kp)
	defer srv.Close()

	provider := NewJWKSProvider(srv.URL, srv.Client())
	mw := MiddlewareWithProvider(provider, srv.URL, "meshsat-hub")
	handler := mw(okHandler())

	token := signJWT(kp, jwt.MapClaims{
		"sub":       "user-123",
		"email":     "test@example.com",
		"name":      "Test User",
		"iss":       srv.URL,
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
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-User-ID"); got != "user-123" {
		t.Errorf("user ID: got %q, want %q", got, "user-123")
	}
	if got := w.Header().Get("X-User-Email"); got != "test@example.com" {
		t.Errorf("user email: got %q, want %q", got, "test@example.com")
	}
	if got := w.Header().Get("X-User-Tenant"); got != "tenant-1" {
		t.Errorf("tenant: got %q, want %q", got, "tenant-1")
	}
}

func TestJWTMiddleware_ExpiredToken(t *testing.T) {
	kp := generateTestKey("key-1")
	srv := startMockOIDCServer(kp)
	defer srv.Close()

	provider := NewJWKSProvider(srv.URL, srv.Client())
	mw := MiddlewareWithProvider(provider, srv.URL, "")
	handler := mw(okHandler())

	token := signJWT(kp, jwt.MapClaims{
		"sub": "user-123",
		"iss": srv.URL,
		"exp": time.Now().Add(-1 * time.Hour).Unix(),
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
	kp := generateTestKey("key-1")
	srv := startMockOIDCServer(kp)
	defer srv.Close()

	provider := NewJWKSProvider(srv.URL, srv.Client())
	mw := MiddlewareWithProvider(provider, srv.URL, "")
	handler := mw(okHandler())

	token := signJWT(kp, jwt.MapClaims{
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

func TestJWTMiddleware_WrongAudience(t *testing.T) {
	kp := generateTestKey("key-1")
	srv := startMockOIDCServer(kp)
	defer srv.Close()

	provider := NewJWKSProvider(srv.URL, srv.Client())
	mw := MiddlewareWithProvider(provider, srv.URL, "meshsat-hub")
	handler := mw(okHandler())

	token := signJWT(kp, jwt.MapClaims{
		"sub": "user-123",
		"iss": srv.URL,
		"aud": "wrong-audience",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})

	req := httptest.NewRequest("GET", "/api/devices", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Errorf("expected 401 for wrong audience, got %d", w.Code)
	}
}

func TestJWTMiddleware_InvalidSignature(t *testing.T) {
	kp := generateTestKey("key-1")
	wrongKey := generateTestKey("key-1") // same kid, different key material
	srv := startMockOIDCServer(kp)
	defer srv.Close()

	provider := NewJWKSProvider(srv.URL, srv.Client())
	mw := MiddlewareWithProvider(provider, srv.URL, "")
	handler := mw(okHandler())

	// Sign with wrong key — JWKS has key-1 mapped to kp, but we sign with wrongKey.
	token := signJWT(wrongKey, jwt.MapClaims{
		"sub": "user-123",
		"iss": srv.URL,
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})

	req := httptest.NewRequest("GET", "/api/devices", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Errorf("expected 401 for forged signature, got %d", w.Code)
	}
}

func TestJWTMiddleware_MissingKid(t *testing.T) {
	kp := generateTestKey("key-1")
	srv := startMockOIDCServer(kp)
	defer srv.Close()

	provider := NewJWKSProvider(srv.URL, srv.Client())
	mw := MiddlewareWithProvider(provider, srv.URL, "")
	handler := mw(okHandler())

	// Create a token without kid in header.
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": "user-123",
		"iss": srv.URL,
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})
	// Deliberately don't set kid.
	signed, _ := token.SignedString(kp.privateKey)

	req := httptest.NewRequest("GET", "/api/devices", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Errorf("expected 401 for missing kid, got %d", w.Code)
	}
}

func TestJWTMiddleware_UnknownKid(t *testing.T) {
	kp := generateTestKey("key-1")
	unknownKey := generateTestKey("key-unknown")
	srv := startMockOIDCServer(kp) // only key-1 in JWKS
	defer srv.Close()

	provider := NewJWKSProvider(srv.URL, srv.Client())
	mw := MiddlewareWithProvider(provider, srv.URL, "")
	handler := mw(okHandler())

	token := signJWT(unknownKey, jwt.MapClaims{
		"sub": "user-123",
		"iss": srv.URL,
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})

	req := httptest.NewRequest("GET", "/api/devices", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Errorf("expected 401 for unknown kid, got %d", w.Code)
	}
}

func TestJWTMiddleware_KeyRotation(t *testing.T) {
	// Start with key-1.
	kp1 := generateTestKey("key-1")
	kp2 := generateTestKey("key-2")

	// Serve both keys in JWKS (simulates rotation — new key added).
	srv := startMockOIDCServer(kp1, kp2)
	defer srv.Close()

	provider := NewJWKSProvider(srv.URL, srv.Client())
	mw := MiddlewareWithProvider(provider, srv.URL, "")
	handler := mw(okHandler())

	// Token signed with new key-2 should work.
	token := signJWT(kp2, jwt.MapClaims{
		"sub": "user-456",
		"iss": srv.URL,
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})

	req := httptest.NewRequest("GET", "/api/devices", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200 for rotated key, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-User-ID"); got != "user-456" {
		t.Errorf("user ID: got %q, want %q", got, "user-456")
	}
}

func TestJWTMiddleware_AudienceArray(t *testing.T) {
	kp := generateTestKey("key-1")
	srv := startMockOIDCServer(kp)
	defer srv.Close()

	provider := NewJWKSProvider(srv.URL, srv.Client())
	mw := MiddlewareWithProvider(provider, srv.URL, "meshsat-hub")
	handler := mw(okHandler())

	// Audience as array (common with some IdPs).
	token := signJWT(kp, jwt.MapClaims{
		"sub": "user-123",
		"iss": srv.URL,
		"aud": []string{"meshsat-hub", "other-service"},
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})

	req := httptest.NewRequest("GET", "/api/devices", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200 for audience array, got %d: %s", w.Code, w.Body.String())
	}
}

func TestJWTMiddleware_ExemptPaths(t *testing.T) {
	kp := generateTestKey("key-1")
	srv := startMockOIDCServer(kp)
	defer srv.Close()

	provider := NewJWKSProvider(srv.URL, srv.Client())
	mw := MiddlewareWithProvider(provider, srv.URL, "")
	handler := mw(okHandler())

	for _, path := range []string{"/healthz", "/readyz", "/", "/api/webhook/rockblock"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Errorf("%s: expected 200, got %d", path, w.Code)
		}
	}
}

func TestJWTMiddleware_MalformedToken(t *testing.T) {
	kp := generateTestKey("key-1")
	srv := startMockOIDCServer(kp)
	defer srv.Close()

	provider := NewJWKSProvider(srv.URL, srv.Client())
	mw := MiddlewareWithProvider(provider, srv.URL, "")
	handler := mw(okHandler())

	for _, tok := range []string{"not-a-jwt", "a.b", "a.b.c.d", ""} {
		name := tok
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/devices", nil)
			if tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != 401 {
				t.Errorf("expected 401, got %d", w.Code)
			}
		})
	}
}

// --- Context and User tests ---

func TestFromContext_Empty(t *testing.T) {
	user := FromContext(context.Background())
	if user != nil {
		t.Error("expected nil user from empty context")
	}
}

func TestFromContext_Nil(t *testing.T) {
	//nolint:staticcheck // intentional nil context test
	user := FromContext(nil)
	if user != nil {
		t.Error("expected nil user from nil context")
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

// --- JWKS provider tests ---

func TestJWKSProvider_Discovery(t *testing.T) {
	kp := generateTestKey("test-kid")
	srv := startMockOIDCServer(kp)
	defer srv.Close()

	provider := NewJWKSProvider(srv.URL, srv.Client())
	if err := provider.RefreshKeys(context.Background()); err != nil {
		t.Fatalf("RefreshKeys: %v", err)
	}

	key, err := provider.GetKey("test-kid")
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if key.N.Cmp(kp.publicKey.N) != 0 {
		t.Error("public key modulus mismatch")
	}
}

func TestJWKSProvider_UnknownKid(t *testing.T) {
	kp := generateTestKey("key-1")
	srv := startMockOIDCServer(kp)
	defer srv.Close()

	provider := NewJWKSProvider(srv.URL, srv.Client())
	_, err := provider.GetKey("nonexistent")
	if err == nil {
		t.Error("expected error for unknown kid")
	}
}

func TestJWKSProvider_NeedsRefresh(t *testing.T) {
	provider := NewJWKSProvider("https://unused", nil)
	if !provider.NeedsRefresh() {
		t.Error("new provider should need refresh")
	}
}

func TestJWKSProvider_BadDiscovery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	provider := NewJWKSProvider(srv.URL, srv.Client())
	err := provider.RefreshKeys(context.Background())
	if err == nil {
		t.Error("expected error for failed discovery")
	}
}

func TestJWKSProvider_EmptyJWKS(t *testing.T) {
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"issuer":"%s","jwks_uri":"%s/jwks"}`, srv.URL, srv.URL)
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"keys":[]}`)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	provider := NewJWKSProvider(srv.URL, srv.Client())
	err := provider.RefreshKeys(context.Background())
	if err == nil {
		t.Error("expected error for empty JWKS")
	}
}

func TestParseRSAPublicKey_Valid(t *testing.T) {
	kp := generateTestKey("test")
	nB64 := base64.RawURLEncoding.EncodeToString(kp.publicKey.N.Bytes())
	eB64 := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(kp.publicKey.E)).Bytes())

	pub, err := parseRSAPublicKey(nB64, eB64)
	if err != nil {
		t.Fatalf("parseRSAPublicKey: %v", err)
	}
	if pub.N.Cmp(kp.publicKey.N) != 0 {
		t.Error("modulus mismatch")
	}
	if pub.E != kp.publicKey.E {
		t.Error("exponent mismatch")
	}
}

func TestExtractUser_AllFields(t *testing.T) {
	claims := jwt.MapClaims{
		"sub":       "uid-42",
		"email":     "alice@example.com",
		"name":      "Alice",
		"tenant_id": "t-1",
		"roles":     []interface{}{"admin", "user"},
	}
	u := extractUser(claims)
	if u.ID != "uid-42" {
		t.Errorf("ID: %q", u.ID)
	}
	if u.Email != "alice@example.com" {
		t.Errorf("Email: %q", u.Email)
	}
	if u.Name != "Alice" {
		t.Errorf("Name: %q", u.Name)
	}
	if u.TenantID != "t-1" {
		t.Errorf("TenantID: %q", u.TenantID)
	}
	if len(u.Roles) != 2 || u.Roles[0] != "admin" {
		t.Errorf("Roles: %v", u.Roles)
	}
}
