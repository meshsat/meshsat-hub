package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// startMockOIDCServerEC serves discovery + a P-256 JWKS. issuerSuffix lets a
// test make the discovery document report the issuer with a trailing slash
// (authentik's spelling) while the caller configures it without one.
func startMockOIDCServerEC(t *testing.T, kid, issuerSuffix string) (*httptest.Server, *ecdsa.PrivateKey) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pad := func(b []byte) string {
		out := make([]byte, 32)
		copy(out[32-len(b):], b)
		return base64.RawURLEncoding.EncodeToString(out)
	}
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 srv.URL + issuerSuffix,
			"jwks_uri":               srv.URL + "/jwks",
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
			"userinfo_endpoint":      srv.URL + "/userinfo",
			"end_session_endpoint":   srv.URL + "/end-session",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "EC", "use": "sig", "kid": kid, "alg": "ES256", "crv": "P-256",
			"x": pad(priv.X.Bytes()), "y": pad(priv.Y.Bytes()),
		}}})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, priv
}

func signES256(t *testing.T, priv *ecdsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestJWKSProvider_ECKeyAndDiscoveryEndpoints(t *testing.T) {
	srv, priv := startMockOIDCServerEC(t, "ec-1", "")
	provider := NewJWKSProvider(srv.URL, srv.Client())

	key, err := provider.GetKey("ec-1")
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	ecKey, ok := key.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("expected *ecdsa.PublicKey, got %T", key)
	}
	if ecKey.X.Cmp(priv.X) != 0 || ecKey.Y.Cmp(priv.Y) != 0 {
		t.Error("EC public key mismatch")
	}
	disc, err := provider.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if disc.AuthorizationEndpoint != srv.URL+"/authorize" || disc.TokenEndpoint != srv.URL+"/token" ||
		disc.UserinfoEndpoint != srv.URL+"/userinfo" || disc.EndSessionEndpoint != srv.URL+"/end-session" {
		t.Errorf("discovery endpoints not parsed: %+v", disc)
	}
}

func TestParseECPublicKey_RejectsBadInput(t *testing.T) {
	if _, err := parseECPublicKey("P-999", "AA", "AA"); err == nil {
		t.Error("unsupported curve must fail")
	}
	if _, err := parseECPublicKey("P-256", "AQ", "AQ"); err == nil {
		t.Error("point off the curve must fail")
	}
}

func TestOIDCMiddleware_ES256Token(t *testing.T) {
	srv, priv := startMockOIDCServerEC(t, "ec-1", "")
	provider := NewJWKSProvider(srv.URL, srv.Client())
	mw := MiddlewareWithProvider(provider, srv.URL, "hub")
	handler := mw(okHandler())

	tok := signES256(t, priv, "ec-1", jwt.MapClaims{
		"iss": srv.URL, "aud": "hub", "sub": "u-ec", "email": "ec@example.org",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	req := httptest.NewRequest("GET", "/api/devices", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 || w.Header().Get("X-User-ID") != "u-ec" {
		t.Fatalf("ES256 token rejected: code=%d body=%s", w.Code, w.Body.String())
	}
}

// TestOIDCMiddleware_IssuerTrailingSlash covers the authentik trap: the
// provider's discovery document (and therefore every token) carries the issuer
// with a trailing slash while HUB_OIDC_ISSUER_URL is configured without one,
// and the reverse.
func TestOIDCMiddleware_IssuerTrailingSlash(t *testing.T) {
	cases := []struct {
		name       string
		configured string // suffix appended to srv.URL for the configured issuer
		discovered string // suffix the discovery document and token use
	}{
		{"configured bare, tokens slashed", "", "/"},
		{"configured slashed, tokens bare", "/", ""},
		{"both slashed", "/", "/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, priv := startMockOIDCServerEC(t, "ec-1", tc.discovered)
			provider := NewJWKSProvider(srv.URL+tc.configured, srv.Client())
			mw := MiddlewareWithProvider(provider, srv.URL+tc.configured, "")
			handler := mw(okHandler())

			tok := signES256(t, priv, "ec-1", jwt.MapClaims{
				"iss": srv.URL + tc.discovered, "sub": "u1", "exp": time.Now().Add(time.Hour).Unix(),
			})
			req := httptest.NewRequest("GET", "/api/devices", nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != 200 {
				t.Fatalf("valid token rejected on issuer spelling: code=%d body=%s", w.Code, w.Body.String())
			}

			// A genuinely different issuer is still rejected.
			bad := signES256(t, priv, "ec-1", jwt.MapClaims{
				"iss": "https://evil.example.org/", "sub": "u1", "exp": time.Now().Add(time.Hour).Unix(),
			})
			req = httptest.NewRequest("GET", "/api/devices", nil)
			req.Header.Set("Authorization", "Bearer "+bad)
			w = httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != 401 {
				t.Fatalf("wrong issuer accepted: code=%d", w.Code)
			}
		})
	}
}

func TestIssuerMatches(t *testing.T) {
	if !IssuerMatches("https://a/x/", "https://a/x") || !IssuerMatches("https://a/x", "https://a/x/") {
		t.Error("trailing slash must be ignored")
	}
	if IssuerMatches("https://a/x", "https://a/y") {
		t.Error("different paths must not match")
	}
}
