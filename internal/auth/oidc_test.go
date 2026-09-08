package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type mapResolver map[string]*User

func (m mapResolver) ResolveSubject(_ context.Context, _, subject string) (*User, error) {
	if u, ok := m[subject]; ok {
		return u, nil
	}
	return nil, errors.New("unknown")
}

func TestOIDCState_RoundTrip_And_Tamper(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	st, err := NewOIDCState("/x", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := EncodeState(st, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeState(enc, key)
	if err != nil || got.State != st.State || got.Nonce != st.Nonce || got.Verifier != st.Verifier || got.Next != "/x" {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if _, err := DecodeState(enc, []byte("another-key-another-key-another!")); err == nil {
		t.Fatal("wrong key accepted")
	}
	// Flip a character in the middle of the MAC. The last character of a
	// RawURLEncoding MAC carries only two payload bits, so replacing it can
	// decode to the same bytes one time in four and the check would flake.
	i := len(enc) - 10
	flip := "A"
	if enc[i] == 'A' {
		flip = "B"
	}
	if _, err := DecodeState(enc[:i]+flip+enc[i+1:], key); err == nil {
		t.Fatal("tampered mac accepted")
	}
	old := &OIDCState{State: "s", Nonce: "n", Verifier: "v", Expires: time.Now().Add(-time.Second).Unix()}
	encOld, _ := EncodeState(old, key)
	if _, err := DecodeState(encOld, key); err == nil {
		t.Fatal("expired state accepted")
	}
	if PKCEChallenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk") != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatal("PKCE S256 vector mismatch (RFC 7636 appendix B)")
	}
}

func TestOIDCMode_SessionThenProviderBearer(t *testing.T) {
	kp := generateTestKey("k1")
	srv := startMockOIDCServer(kp)
	defer srv.Close()
	secret := []byte("0123456789abcdef0123456789abcdef")
	resolver := mapResolver{"sub-known": {ID: "usr_1", Email: "k@example.com", Roles: []string{"operator"}, TenantID: "t_1", PlatformAdmin: false}}
	mw := Middleware(Config{Mode: "oidc", OIDCIssuerURL: srv.URL, OIDCAudience: "meshsat-hub", JWTSecret: secret, Resolver: resolver, Token: "legacy"})
	h := mw(okHandler())

	do := func(bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/devices", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	// 1. Hub session token issued by the callback.
	sm := NewSessionManager(secret, "meshsat-hub")
	tok, _ := sm.IssueAccessTokenFor("usr_9", "s@example.com", "S", "owner", "t_9", true)
	if rr := do(tok); rr.Code != 200 || rr.Header().Get("X-User-Tenant") != "t_9" || rr.Header().Get("X-User-ID") != "usr_9" {
		t.Fatalf("session token: %d %v", rr.Code, rr.Header())
	}
	// 2. Provider bearer with a known subject: role/tenant from the resolver, not the claims.
	claims := jwt.MapClaims{"iss": srv.URL, "sub": "sub-known", "aud": "meshsat-hub", "exp": time.Now().Add(time.Hour).Unix(), "tenant_id": "t_claimed", "roles": []string{"admin"}}
	if rr := do(signJWT(kp, claims)); rr.Code != 200 || rr.Header().Get("X-User-Tenant") != "t_1" || rr.Header().Get("X-User-ID") != "usr_1" {
		t.Fatalf("provider bearer: %d %v", rr.Code, rr.Header())
	}
	// 3. Provider bearer with an unknown subject: refused.
	claims["sub"] = "sub-unknown"
	if rr := do(signJWT(kp, claims)); rr.Code != 401 {
		t.Fatalf("unknown subject: %d", rr.Code)
	}
	// 4. Legacy static token still works; garbage does not; exempt path open.
	if rr := do("legacy"); rr.Code != 200 {
		t.Fatalf("legacy token: %d", rr.Code)
	}
	if rr := do("garbage"); rr.Code != 401 {
		t.Fatalf("garbage: %d", rr.Code)
	}
	req := httptest.NewRequest("GET", "/api/auth/config", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("exempt: %d", rr.Code)
	}
	// 5. Without a resolver, provider bearers are refused even when valid.
	mw2 := Middleware(Config{Mode: "oidc", OIDCIssuerURL: srv.URL, OIDCAudience: "meshsat-hub", JWTSecret: secret})
	claims["sub"] = "sub-known"
	req = httptest.NewRequest("GET", "/api/devices", nil)
	req.Header.Set("Authorization", "Bearer "+signJWT(kp, claims))
	rr = httptest.NewRecorder()
	mw2(okHandler()).ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("no resolver: %d", rr.Code)
	}
}

func TestSessionClaims_PlatformAdmin(t *testing.T) {
	sm := NewSessionManager([]byte("0123456789abcdef0123456789abcdef"), "meshsat-hub")
	tok, err := sm.IssueAccessTokenFor("u", "e", "n", "owner", "t", true)
	if err != nil {
		t.Fatal(err)
	}
	c, err := sm.VerifyAccessToken(tok)
	if err != nil || !c.PlatformAdmin {
		t.Fatalf("claims %+v err %v", c, err)
	}
	tok, _ = sm.IssueAccessToken("u", "e", "n", "owner", "t")
	c, _ = sm.VerifyAccessToken(tok)
	if c.PlatformAdmin {
		t.Fatal("default must not be admin")
	}
	_ = http.StatusOK
}
