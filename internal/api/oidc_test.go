package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// fakeIdP is a minimal OpenID provider: discovery, JWKS, authorize (records
// the request), token (returns an ID token built from the next claims).
type fakeIdP struct {
	srv       *httptest.Server
	key       *rsa.PrivateKey
	clientID  string
	secret    string
	issuer    string // what the id_token says; defaults to srv.URL
	nextClaim jwt.MapClaims
	lastAuthz url.Values
	lastToken url.Values
	tokenFail bool
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{key: key, clientID: "meshsat-hub", secret: "s3cret"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer": f.srv.URL + "/", "jwks_uri": f.srv.URL + "/jwks",
			"authorization_endpoint": f.srv.URL + "/authorize", "token_endpoint": f.srv.URL + "/token",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"keys": []map[string]string{{
			"kty": "RSA", "use": "sig", "kid": "k1", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		f.lastAuthz = r.URL.Query()
		w.WriteHeader(200)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.lastToken = r.PostForm
		u, p, ok := r.BasicAuth()
		if !ok || u != f.clientID || p != f.secret || f.tokenFail {
			w.WriteHeader(401)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
			return
		}
		claims := jwt.MapClaims{}
		for k, v := range f.nextClaim {
			claims[k] = v
		}
		iss := f.issuer
		if iss == "" {
			iss = f.srv.URL + "/"
		}
		claims["iss"] = iss
		if _, ok := claims["aud"]; !ok {
			claims["aud"] = f.clientID
		}
		claims["exp"] = time.Now().Add(time.Hour).Unix()
		claims["iat"] = time.Now().Unix()
		if n := r.PostForm.Get("nonce"); n != "" {
			claims["nonce"] = n
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = "k1"
		signed, _ := tok.SignedString(key)
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": signed, "access_token": "at", "token_type": "Bearer"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

type oidcEnv struct {
	idp     *fakeIdP
	store   store.Store
	handler *OIDCHandler
	sm      *hubauth.SessionManager
	cfg     OIDCConfig
}

func newOIDCEnv(t *testing.T, tenantEnforce bool) *oidcEnv {
	t.Helper()
	s, err := sqlite.New(":memory:", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	idp := newFakeIdP(t)
	key := []byte("0123456789abcdef0123456789abcdef")
	sm := hubauth.NewSessionManager(key, "meshsat-hub")
	login := NewLoginHandler(s, sm, nil)
	provider := hubauth.NewJWKSProvider(idp.srv.URL, nil)
	client := &hubauth.OIDCClient{Provider: provider, ClientID: idp.clientID, ClientSecret: idp.secret, RedirectURI: "https://hub.example/api/auth/oidc/callback"}
	cfg := OIDCConfig{AdminGroup: "meshsat-platform-admin", StateKey: key, TenantEnforce: tenantEnforce, BootstrapOwnerEmail: "owner@example.com", SignupURL: "https://auth.example/if/flow/meshsat-enrollment/", CommunityURL: "https://matrix.to/#/#meshsat:example.org"}
	h := NewOIDCHandler(s, login, client, cfg, []string{"oidc"})
	return &oidcEnv{idp: idp, store: s, handler: h, sm: sm, cfg: cfg}
}

// startLogin performs GET /login and returns the state cookie and the
// state/nonce sent to the IdP.
func (e *oidcEnv) startLogin(t *testing.T, next string) (*http.Cookie, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/auth/oidc/login?next="+url.QueryEscape(next), nil)
	rr := httptest.NewRecorder()
	e.handler.Login(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("login status %d: %s", rr.Code, rr.Body.String())
	}
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == oidcStateCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no state cookie")
	}
	if cookie.SameSite != http.SameSiteLaxMode || !cookie.HttpOnly || cookie.Path != "/api/auth/oidc" {
		t.Fatalf("cookie attributes: %+v", cookie)
	}
	q := loc.Query()
	e.idp.lastAuthz = q // the test never follows the redirect; record what the IdP would see
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" {
		t.Fatalf("authorize params missing PKCE/nonce: %v", q)
	}
	return cookie, q.Get("state")
}

func (e *oidcEnv) callback(t *testing.T, cookie *http.Cookie, state string, claims jwt.MapClaims) *httptest.ResponseRecorder {
	t.Helper()
	e.idp.nextClaim = claims
	req := httptest.NewRequest("GET", "/api/auth/oidc/callback?code=abc&state="+url.QueryEscape(state), nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	// The token endpoint echoes the nonce it receives; the client does not
	// send it, so inject the state's nonce into the IdP from the cookie.
	if cookie != nil {
		if st, err := hubauth.DecodeState(cookie.Value, e.cfg.StateKey); err == nil {
			e.idp.nextClaim["nonce"] = st.Nonce
		}
	}
	e.handler.Callback(rr, req)
	return rr
}

func location(rr *httptest.ResponseRecorder) string { return rr.Header().Get("Location") }

func refreshCookie(rr *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rr.Result().Cookies() {
		if c.Name == "meshsat_refresh" && c.Value != "" {
			return c
		}
	}
	return nil
}

func TestOIDC_NewUser_CreatesTenantAndOwner(t *testing.T) {
	e := newOIDCEnv(t, true)
	cookie, state := e.startLogin(t, "/devices")
	rr := e.callback(t, cookie, state, jwt.MapClaims{"sub": "u-1", "email": "Alice@Example.com", "email_verified": true, "name": "Alice", "groups": []string{"meshsat-viewer"}})
	if rr.Code != http.StatusFound || !strings.HasPrefix(location(rr), oidcCallbackRoute) || strings.Contains(location(rr), "error=") {
		t.Fatalf("status %d location %q body %s", rr.Code, location(rr), rr.Body.String())
	}
	if !strings.Contains(location(rr), "next=%2Fdevices") {
		t.Fatalf("next not propagated: %s", location(rr))
	}
	if strings.Contains(location(rr), "access_token") {
		t.Fatal("tokens must not appear in the redirect URL")
	}
	rc := refreshCookie(rr)
	if rc == nil {
		t.Fatal("no refresh cookie")
	}
	ident, err := e.store.GetOIDCIdentity(context.Background(), e.idp.srv.URL+"/", "u-1")
	if err != nil {
		t.Fatalf("identity not linked: %v", err)
	}
	if ident.TenantID == "default" || ident.Email != "alice@example.com" {
		t.Fatalf("identity %+v", ident)
	}
	tenant, err := e.store.GetTenant(context.Background(), ident.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	if tenant.Slug != "alice" || tenant.OwnerUserID != ident.UserID {
		t.Fatalf("tenant %+v", tenant)
	}
	// A tenant that provisions itself lands on the free tier (MESHSAT-989).
	// It used to land on "beta", which now means an unlimited fleet: everyone
	// who signed themselves up would have been grandfathered into a plan they
	// never asked for and nobody was ever going to notice.
	if tenant.Plan != plans.Free {
		t.Errorf("a self-provisioned tenant is on %q, want %q", tenant.Plan, plans.Free)
	}
	u, err := e.store.GetUserByID(context.Background(), ident.TenantID, ident.UserID)
	if err != nil || u.Role != hubauth.RoleOwner {
		t.Fatalf("user %+v err %v", u, err)
	}
	// Second login re-links to the same user and tenant.
	cookie, state = e.startLogin(t, "")
	rr = e.callback(t, cookie, state, jwt.MapClaims{"sub": "u-1", "email": "alice@example.com", "email_verified": true, "groups": []string{"meshsat-operator"}})
	if strings.Contains(location(rr), "error=") {
		t.Fatalf("second login failed: %s", location(rr))
	}
	tenants, _ := e.store.ListTenants(context.Background())
	if len(tenants) != 2 { // default + alice
		t.Fatalf("tenants: %d", len(tenants))
	}
	u, _ = e.store.GetUserByID(context.Background(), ident.TenantID, ident.UserID)
	if u.Role != hubauth.RoleOperator {
		t.Fatalf("role not refreshed from groups: %s", u.Role)
	}
}

func TestOIDC_NoGroup_PendingApproval_CreatesNothing(t *testing.T) {
	e := newOIDCEnv(t, true)
	cookie, state := e.startLogin(t, "")
	rr := e.callback(t, cookie, state, jwt.MapClaims{"sub": "u-2", "email": "bob@example.com", "email_verified": true, "groups": []string{"meshsat-pending"}})
	if !strings.Contains(location(rr), "error=pending_approval") {
		t.Fatalf("location %q", location(rr))
	}
	if refreshCookie(rr) != nil {
		t.Fatal("session issued for unapproved user")
	}
	if _, err := e.store.GetOIDCIdentity(context.Background(), e.idp.srv.URL+"/", "u-2"); err == nil {
		t.Fatal("identity created for unapproved user")
	}
	tenants, _ := e.store.ListTenants(context.Background())
	if len(tenants) != 1 {
		t.Fatalf("tenant created for unapproved user: %d", len(tenants))
	}
}

func TestOIDC_BootstrapOwner_AttachesToDefault(t *testing.T) {
	e := newOIDCEnv(t, true)
	cookie, state := e.startLogin(t, "")
	rr := e.callback(t, cookie, state, jwt.MapClaims{"sub": "u-3", "email": "owner@example.com", "email_verified": true, "groups": []string{"meshsat-platform-admin"}})
	if strings.Contains(location(rr), "error=") {
		t.Fatalf("location %q", location(rr))
	}
	ident, err := e.store.GetOIDCIdentity(context.Background(), e.idp.srv.URL+"/", "u-3")
	if err != nil {
		t.Fatal(err)
	}
	if ident.TenantID != "default" || !ident.PlatformAdmin {
		t.Fatalf("identity %+v", ident)
	}
	// Session claims carry the admin flag.
	c := rr.Result().Cookies()
	_ = c
	u, _ := e.store.GetUserByID(context.Background(), "default", ident.UserID)
	if u.Role != hubauth.RoleOwner {
		t.Fatalf("role %s", u.Role)
	}
	// Provider bearer resolution uses the linked identity.
	got, err := e.handler.ResolveSubject(context.Background(), e.idp.srv.URL+"/", "u-3")
	if err != nil || got.TenantID != "default" || !got.PlatformAdmin || !got.HasRole(hubauth.RoleOwner) {
		t.Fatalf("resolve: %+v err %v", got, err)
	}
	if _, err := e.handler.ResolveSubject(context.Background(), e.idp.srv.URL+"/", "nobody"); err == nil {
		t.Fatal("unknown subject resolved")
	}
}

func TestOIDC_Invite_JoinsTenant(t *testing.T) {
	e := newOIDCEnv(t, true)
	ctx := context.Background()
	if err := e.store.CreateTenant(ctx, &store.Tenant{ID: "t_acme", Slug: "acme", Name: "ACME", Plan: "beta", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	inv := &store.TenantInvite{ID: "inv1", TenantID: "t_acme", Email: "carol@example.com", Role: hubauth.RoleOperator, TokenHash: "h", ExpiresAt: time.Now().Add(time.Hour)}
	if err := e.store.CreateInvite(ctx, "t_acme", inv); err != nil {
		t.Fatal(err)
	}
	cookie, state := e.startLogin(t, "")
	rr := e.callback(t, cookie, state, jwt.MapClaims{"sub": "u-4", "email": "carol@example.com", "email_verified": true, "groups": []string{"meshsat-viewer"}})
	if strings.Contains(location(rr), "error=") {
		t.Fatalf("location %q", location(rr))
	}
	ident, _ := e.store.GetOIDCIdentity(ctx, e.idp.srv.URL+"/", "u-4")
	if ident == nil || ident.TenantID != "t_acme" {
		t.Fatalf("identity %+v", ident)
	}
	u, _ := e.store.GetUserByID(ctx, "t_acme", ident.UserID)
	if u.Role != hubauth.RoleOperator {
		t.Fatalf("invite role not applied: %s", u.Role)
	}
	if p, err := e.store.GetPendingInviteByEmail(ctx, "carol@example.com"); err == nil && p != nil {
		t.Fatal("invite still pending")
	}
}

func TestOIDC_UnverifiedEmail_Refused(t *testing.T) {
	e := newOIDCEnv(t, true)
	cookie, state := e.startLogin(t, "")
	rr := e.callback(t, cookie, state, jwt.MapClaims{"sub": "u-5", "email": "dave@example.com", "email_verified": false, "groups": []string{"meshsat-viewer"}})
	if !strings.Contains(location(rr), "error=provision") {
		t.Fatalf("location %q", location(rr))
	}
}

func TestOIDC_StateMismatch(t *testing.T) {
	e := newOIDCEnv(t, true)
	cookie, _ := e.startLogin(t, "")
	claims := jwt.MapClaims{"sub": "u-6", "email": "eve@example.com", "email_verified": true, "groups": []string{"meshsat-viewer"}}
	if rr := e.callback(t, cookie, "wrong", claims); !strings.Contains(location(rr), "error=state") {
		t.Fatalf("wrong state accepted: %q", location(rr))
	}
	_, state := e.startLogin(t, "")
	if rr := e.callback(t, nil, state, claims); !strings.Contains(location(rr), "error=state") {
		t.Fatalf("missing cookie accepted: %q", location(rr))
	}
	tampered := *cookie
	tampered.Value = cookie.Value[:len(cookie.Value)-2] + "xx"
	if rr := e.callback(t, &tampered, state, claims); !strings.Contains(location(rr), "error=state") {
		t.Fatalf("tampered cookie accepted: %q", location(rr))
	}
}

func TestOIDC_NonceMismatch_Rejected(t *testing.T) {
	e := newOIDCEnv(t, true)
	cookie, state := e.startLogin(t, "")
	e.idp.nextClaim = jwt.MapClaims{"sub": "u-7", "email": "f@example.com", "email_verified": true, "groups": []string{"meshsat-viewer"}}
	req := httptest.NewRequest("GET", "/api/auth/oidc/callback?code=abc&state="+url.QueryEscape(state), nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	e.idp.nextClaim["nonce"] = "not-the-nonce"
	e.handler.Callback(rr, req)
	if !strings.Contains(location(rr), "error=token") {
		t.Fatalf("nonce mismatch accepted: %q", location(rr))
	}
}

func TestOIDC_IssuerTrailingSlash_And_WrongIssuer(t *testing.T) {
	e := newOIDCEnv(t, true)
	claims := jwt.MapClaims{"sub": "u-8", "email": "g@example.com", "email_verified": true, "groups": []string{"meshsat-viewer"}}
	// Discovery says issuer with slash; token without slash: accepted.
	e.idp.issuer = e.idp.srv.URL
	cookie, state := e.startLogin(t, "")
	if rr := e.callback(t, cookie, state, claims); strings.Contains(location(rr), "error=") {
		t.Fatalf("trailing-slash issuer rejected: %q", location(rr))
	}
	e.idp.issuer = "https://evil.example/"
	cookie, state = e.startLogin(t, "")
	if rr := e.callback(t, cookie, state, claims); !strings.Contains(location(rr), "error=token") {
		t.Fatalf("wrong issuer accepted: %q", location(rr))
	}
}

func TestOIDC_WrongAudience_Rejected(t *testing.T) {
	e := newOIDCEnv(t, true)
	cookie, state := e.startLogin(t, "")
	rr := e.callback(t, cookie, state, jwt.MapClaims{"sub": "u-9", "aud": "other-client", "email": "h@example.com", "email_verified": true, "groups": []string{"meshsat-viewer"}})
	if !strings.Contains(location(rr), "error=token") {
		t.Fatalf("wrong audience accepted: %q", location(rr))
	}
}

func TestOIDC_ExchangeFailure_And_ProviderError(t *testing.T) {
	e := newOIDCEnv(t, true)
	cookie, state := e.startLogin(t, "")
	e.idp.tokenFail = true
	if rr := e.callback(t, cookie, state, jwt.MapClaims{"sub": "x"}); !strings.Contains(location(rr), "error=exchange") {
		t.Fatalf("exchange failure: %q", location(rr))
	}
	e.idp.tokenFail = false
	cookie, state = e.startLogin(t, "")
	req := httptest.NewRequest("GET", "/api/auth/oidc/callback?error=access_denied&state="+url.QueryEscape(state), nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	e.handler.Callback(rr, req)
	if !strings.Contains(location(rr), "error=denied") {
		t.Fatalf("provider error: %q", location(rr))
	}
}

func TestOIDC_PKCEVerifierSent(t *testing.T) {
	e := newOIDCEnv(t, true)
	cookie, state := e.startLogin(t, "")
	e.callback(t, cookie, state, jwt.MapClaims{"sub": "u-10", "email": "i@example.com", "email_verified": true, "groups": []string{"meshsat-viewer"}})
	v := e.idp.lastToken.Get("code_verifier")
	if v == "" || hubauth.PKCEChallenge(v) != e.idp.lastAuthz.Get("code_challenge") {
		t.Fatalf("verifier %q does not match challenge %q", v, e.idp.lastAuthz.Get("code_challenge"))
	}
	if e.idp.lastToken.Get("redirect_uri") != "https://hub.example/api/auth/oidc/callback" {
		t.Fatalf("redirect_uri %q", e.idp.lastToken.Get("redirect_uri"))
	}
}

func TestOIDC_SafeNext(t *testing.T) {
	for in, want := range map[string]string{"/devices": "/devices", "//evil.example": "", "https://evil.example": "", "": "", "/a\r\nSet-Cookie: x": "", "/\\evil": ""} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q)=%q want %q", in, got, want)
		}
	}
}

func TestOIDC_TenantEnforceOff_UsesDefault(t *testing.T) {
	e := newOIDCEnv(t, false)
	cookie, state := e.startLogin(t, "")
	rr := e.callback(t, cookie, state, jwt.MapClaims{"sub": "u-11", "email": "j@example.com", "email_verified": true, "groups": []string{"meshsat-viewer"}})
	if strings.Contains(location(rr), "error=") {
		t.Fatalf("location %q", location(rr))
	}
	ident, _ := e.store.GetOIDCIdentity(context.Background(), e.idp.srv.URL+"/", "u-11")
	if ident == nil || ident.TenantID != "default" {
		t.Fatalf("identity %+v", ident)
	}
}

func TestAuthConfig(t *testing.T) {
	e := newOIDCEnv(t, true)
	rr := httptest.NewRecorder()
	e.handler.Config(rr, httptest.NewRequest("GET", "/api/auth/config", nil))
	var resp authConfigResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.OIDCLoginURL != "/api/auth/oidc/login" || len(resp.Modes) != 1 || resp.Modes[0] != "oidc" {
		t.Fatalf("%+v", resp)
	}
	if resp.SignupURL == "" || resp.CommunityURL == "" {
		t.Fatalf("public links missing: %+v", resp)
	}
	rr = httptest.NewRecorder()
	AuthConfigHandler([]string{"local"}, "https://matrix.to/#/#meshsat:example.org").ServeHTTP(rr, httptest.NewRequest("GET", "/api/auth/config", nil))
	var local authConfigResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &local)
	if local.SignupURL != "" || local.CommunityURL == "" || local.Modes[0] != "local" {
		t.Fatalf("local config: %+v", local)
	}
}

func TestMetricsTokenGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	rr := httptest.NewRecorder()
	MetricsTokenGuard("", ok).ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	if rr.Code != 200 {
		t.Fatal("no token should be open")
	}
	g := MetricsTokenGuard("tok", ok)
	rr = httptest.NewRecorder()
	g.ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	if rr.Code != 401 {
		t.Fatal("missing bearer accepted")
	}
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rr = httptest.NewRecorder()
	g.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatal("valid bearer rejected")
	}
}
