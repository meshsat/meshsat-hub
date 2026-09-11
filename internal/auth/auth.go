// Package auth provides authentication middleware for the Hub API.
// Supports three modes:
// - "none": no authentication (development only)
// - "token": simple bearer token (standalone, HUB_AUTH_TOKEN)
// - "oidc": JWT signature verification against an OIDC provider's JWKS (cluster/k8s)
package auth

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/meshsat/meshsat-hub/internal/tlspin"
)

type contextKey string

const UserContextKey contextKey = "auth_user"
const TenantContextKey contextKey = "tenant_id"

// User represents an authenticated user or API client.
type User struct {
	ID            string    `json:"id"`
	Email         string    `json:"email,omitempty"`
	Name          string    `json:"name,omitempty"`
	Roles         []string  `json:"roles,omitempty"`
	TenantID      string    `json:"tenant_id,omitempty"`
	PlatformAdmin bool      `json:"platform_admin,omitempty"` // may act across tenants (X-Tenant-ID)
	ExpiresAt     time.Time `json:"-"`                        // API key expiry (zero = no expiry)
}

// HasRole returns true if the user has the specified role.
func (u *User) HasRole(role string) bool {
	for _, r := range u.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// FromContext extracts the authenticated user from the request context.
func FromContext(ctx context.Context) *User {
	if ctx == nil {
		return nil
	}
	u, _ := ctx.Value(UserContextKey).(*User)
	return u
}

// TenantIDFromContext returns the tenant ID from context, or "default" if none is set.
func TenantIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return "default"
	}
	if tid, ok := ctx.Value(TenantContextKey).(string); ok && tid != "" {
		return tid
	}
	return "default"
}

// TenantMiddleware resolves the tenant ID from the authenticated user and injects
// it into the request context. Resolution order:
//  1. X-Tenant-ID header, platform admins only
//  2. User.TenantID, set by the auth middleware from the JWT claim or the
//     users row -- and by the API key middleware from the key's tenant
//  3. A tenant an earlier authenticated middleware left in the context
//  4. Falls back to "default" (single-tenant compatibility)
//
// If enforce is true, requests without a resolvable tenant get a 403 response.
// TenantStatusLookup reports a tenant's lifecycle status. Supplying one to
// TenantMiddleware makes suspension and deletion take effect: without it the
// status column is a label nothing reads, which is what it was until
// offboarding needed it to mean something.
type TenantStatusLookup func(ctx context.Context, tenantID string) (status string, err error)

var tenantStatus TenantStatusLookup

// SetTenantStatusLookup wires the status check. Called once at startup.
func SetTenantStatusLookup(f TenantStatusLookup) { tenantStatus = f }

func TenantMiddleware(enforce bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip tenant resolution for exempt paths.
			if isExempt(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			var tenantID string
			u := FromContext(r.Context())

			// 1. Platform admins may act on any tenant via X-Tenant-ID.
			if u != nil && u.PlatformAdmin {
				if h := strings.TrimSpace(r.Header.Get("X-Tenant-ID")); h != "" {
					tenantID = h
				}
			}

			// 2. From the authenticated user (JWT claim / users row). The
			// header is never trusted for anyone else (MESHSAT-916).
			if tenantID == "" && u != nil && u.TenantID != "" {
				tenantID = u.TenantID
			}

			// 3. A tenant an earlier trusted middleware already resolved into
			// the context (the API key chain). Belt and braces behind step 2:
			// this value cannot come from a header, only from middleware that
			// has already authenticated the caller, so honouring it is safe --
			// and it means a resolver that forgets to fill User.TenantID
			// degrades to nothing rather than to the default tenant's rows.
			if tenantID == "" {
				if tid, ok := r.Context().Value(TenantContextKey).(string); ok && tid != "" {
					tenantID = tid
				}
			}

			// 4. Default fallback.
			if tenantID == "" {
				if enforce {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					_, _ = fmt.Fprintf(w, `{"error":"tenant context required"}`)
					return
				}
				tenantID = "default"
			}

			// A suspended or deleted tenant is refused everything. Deletion is
			// reversible for a grace period, so this is what makes "blocked
			// immediately, destroyed later" true rather than aspirational.
			if tenantStatus != nil {
				switch st, err := tenantStatus(r.Context(), tenantID); {
				case err != nil:
					// Fail open on a lookup error: a database blip must not
					// lock every tenant out of a running system.
					slog.Warn("tenant status lookup failed", "tenant", tenantID, "error", err)
				case st == "suspended":
					writeTenantBlocked(w, "tenant suspended")
					return
				case st == "deleted":
					writeTenantBlocked(w, "tenant deleted")
					return
				}
			}

			ctx := context.WithValue(r.Context(), TenantContextKey, tenantID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func writeTenantBlocked(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = fmt.Fprintf(w, `{"error":%q}`, msg)
}

// Config holds authentication configuration.
type Config struct {
	Mode              string // "none", "token", "oidc", "local"
	Token             string // static bearer token (mode=token)
	OIDCIssuerURL     string // OIDC issuer URL (mode=oidc)
	OIDCAudience      string // expected JWT audience
	JWTSecret         []byte // HMAC-SHA256 key (mode=local)
	OIDCCertPin       string // base64-encoded SHA-256 SPKI hash for OIDC provider cert pin
	OIDCCertPinBackup string // backup pin for zero-downtime rotation

	// Provider is an already constructed JWKS provider (mode=oidc); when nil
	// one is built from OIDCIssuerURL.
	Provider *JWKSProvider
	// Resolver maps a verified provider subject to the local user (role and
	// tenant come from the users table, never from provider claims). When nil,
	// provider bearer tokens are rejected and only Hub sessions are accepted.
	Resolver SubjectResolver
}

// SubjectResolver looks up the local user for an IdP (issuer, subject).
type SubjectResolver interface {
	ResolveSubject(ctx context.Context, issuer, subject string) (*User, error)
}

// Middleware returns an HTTP middleware that authenticates requests.
func Middleware(cfg Config) func(http.Handler) http.Handler {
	switch cfg.Mode {
	case "oidc":
		slog.Info("auth: OIDC mode", "issuer", cfg.OIDCIssuerURL)
		provider := cfg.Provider
		if provider == nil {
			provider = NewJWKSProvider(cfg.OIDCIssuerURL, OIDCHTTPClient(cfg))
		}
		if len(cfg.JWTSecret) >= 32 {
			// Hub sessions (issued by the OIDC callback) first, then provider
			// bearers resolved through the users table.
			sm := NewSessionManager(cfg.JWTSecret, "meshsat-hub")
			return sessionOrProviderMiddleware(sm, cfg.Token, provider, cfg.OIDCIssuerURL, cfg.OIDCAudience, cfg.Resolver)
		}
		return jwtMiddleware(provider, cfg.OIDCIssuerURL, cfg.OIDCAudience)
	case "token":
		slog.Info("auth: token mode")
		return tokenMiddleware(cfg.Token)
	case "local":
		slog.Info("auth: local mode (built-in user accounts)")
		return localMiddleware(cfg.JWTSecret, cfg.Token)
	default:
		slog.Warn("auth: no authentication (mode=none)")
		return noopMiddleware()
	}
}

// localMiddleware accepts local JWT access tokens signed by the SessionManager.
// Also accepts the legacy static token for backward compatibility during migration.
func localMiddleware(jwtSecret []byte, legacyToken string) func(http.Handler) http.Handler {
	sm := NewSessionManager(jwtSecret, "meshsat-hub")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isExempt(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			// Already authenticated by API key middleware
			if FromContext(r.Context()) != nil {
				next.ServeHTTP(w, r)
				return
			}

			provided := extractBearer(r)
			if provided == "" {
				writeAuthError(w, "missing Authorization header")
				return
			}

			// Try legacy static token first (backward compat during migration)
			if legacyToken != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(legacyToken)) == 1 {
				user := &User{ID: "token-user", Name: "API Token", Roles: []string{"admin"}, PlatformAdmin: true}
				ctx := context.WithValue(r.Context(), UserContextKey, user)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			// Try local JWT
			claims, err := sm.VerifyAccessToken(provided)
			if err != nil {
				writeAuthError(w, "invalid token")
				return
			}

			user := &User{
				ID:            claims.UserID,
				Email:         claims.Email,
				Name:          claims.Name,
				Roles:         []string{claims.Role},
				TenantID:      claims.TenantID,
				PlatformAdmin: claims.PlatformAdmin,
			}
			ctx := context.WithValue(r.Context(), UserContextKey, user)
			if claims.TenantID != "" {
				ctx = context.WithValue(ctx, TenantContextKey, claims.TenantID)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// MiddlewareWithProvider returns OIDC middleware using an externally provided JWKSProvider.
// This is useful for testing and for sharing a provider across components.
func MiddlewareWithProvider(provider *JWKSProvider, issuerURL, audience string) func(http.Handler) http.Handler {
	return jwtMiddleware(provider, issuerURL, audience)
}

// publicPrefixes are the non-API paths served without authentication: the SPA
// and the assets a browser needs to render it before anyone has signed in.
//
// This list is deliberately explicit. It replaces `!strings.HasPrefix(path,
// "/api/")`, which exempted every path that was not an API route -- and so
// silently exempted /metrics, /startupz, /debug/pprof/* and the whole basemap
// as well as the SPA. That was survivable behind an IP allowlist and is not
// survivable in public: pprof heap dumps carry access tokens, refresh tokens
// and message plaintext, and the comment at its route claimed it was behind
// auth. Anything not named here now falls through to the auth chain.
var publicPrefixes = []string{
	"/assets/",  // SPA bundle
	"/basemap/", // self-hosted map tiles, glyphs and sprites
	"/favicon",  // favicon.ico and friends
	"/meshsat-", // brand marks referenced by the login page
	"/fonts/",   //
}

// publicExact are single non-API paths served without authentication.
var publicExact = map[string]bool{
	"/":                 true, // the SPA entry point
	"/index.html":       true,
	"/healthz":          true,
	"/readyz":           true,
	"/startupz":         true,
	"/manifest.json":    true,
	"/robots.txt":       true,
	"/site.webmanifest": true,
}

func isExempt(path string) bool {
	switch {
	case strings.HasPrefix(path, "/api/webhook/"): // all inbound webhooks are auth-exempt
		return true
	case isProvisionClaim(path): // QR provision claim — nonce IS the auth (MESHSAT-414)
		return true
	case path == "/donate":
		// The public donate link, followed from meshsat.net. Exempt for the
		// same reason as /api/donate below; named explicitly rather than left
		// to the single-page-app heuristic further down, which would exempt it
		// only by accident of having no dot in it.
		return true
	case path == "/api/donate":
		// A gift from somebody with no account. There is nothing to
		// authenticate: the caller is a stranger by definition and the money
		// is verified by Stripe, not by us. Rate limited in main.go, and it
		// creates a Checkout session and nothing else -- no tenant is read and
		// none can be named by the caller.
		return true
	case path == "/api/auth/login",
		path == "/api/auth/refresh",
		path == "/api/auth/config",
		path == "/api/auth/oidc/login",
		path == "/api/auth/oidc/callback":
		return true
	case strings.HasPrefix(path, "/api/"):
		return false
	}
	// Non-API paths: only what the SPA needs before sign-in. /metrics is
	// absent on purpose -- it carries its own token guard and must not also be
	// blanket-exempt, so a lost token does not leave it wide open.
	if publicExact[path] {
		return true
	}
	for _, p := range publicPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	// Nothing under these is ever the SPA, whatever it looks like. Without this
	// the client-side-route heuristic below would exempt /debug/pprof/heap,
	// whose last segment has no extension.
	for _, p := range []string{"/debug/", "/api/"} {
		if strings.HasPrefix(path, p) {
			return false
		}
	}
	// The SPA is a single page app served from a catch-all route, so any path
	// that looks like a client-side route (no dot in the last segment) is the
	// index document. A path with an extension is a real asset request and must
	// have matched a prefix above.
	last := path[strings.LastIndex(path, "/")+1:]
	return !strings.Contains(last, ".")
}

// isProvisionClaim matches GET /api/bridges/{id}/provision/{nonce} —
// the nonce acts as a single-use bearer token, no auth header needed.
func isProvisionClaim(path string) bool {
	// Pattern: /api/bridges/SOMETHING/provision/SOMETHING
	if !strings.HasPrefix(path, "/api/bridges/") {
		return false
	}
	parts := strings.Split(path, "/")
	// /api/bridges/{id}/provision/{nonce} = 6 parts (empty, api, bridges, id, provision, nonce)
	return len(parts) == 6 && parts[4] == "provision"
}

func noopMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := &User{ID: "anonymous", Name: "Anonymous", Roles: []string{"admin"}}
			ctx := context.WithValue(r.Context(), UserContextKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func tokenMiddleware(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isExempt(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			// Already authenticated (e.g. by API key middleware).
			if FromContext(r.Context()) != nil {
				next.ServeHTTP(w, r)
				return
			}

			provided := extractBearer(r)
			if provided == "" {
				writeAuthError(w, "missing Authorization header")
				return
			}
			if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
				writeAuthError(w, "invalid token")
				return
			}

			user := &User{ID: "token-user", Name: "API Token", Roles: []string{"admin"}, PlatformAdmin: true}
			ctx := context.WithValue(r.Context(), UserContextKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func jwtMiddleware(provider *JWKSProvider, issuerURL, audience string) func(http.Handler) http.Handler {
	// Build parser options. The issuer is checked after parsing (see below)
	// rather than with jwt.WithIssuer: authentik reports its issuer with a
	// trailing slash while the configured URL usually has none, and the
	// authoritative value is only known after discovery.
	opts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512"}),
		jwt.WithExpirationRequired(),
	}
	if audience != "" {
		opts = append(opts, jwt.WithAudience(audience))
	}

	parser := jwt.NewParser(opts...)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isExempt(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			// Already authenticated (e.g. by API key middleware).
			if FromContext(r.Context()) != nil {
				next.ServeHTTP(w, r)
				return
			}

			tokenStr := extractBearer(r)
			if tokenStr == "" {
				writeAuthError(w, "missing Authorization header")
				return
			}

			// Parse and verify JWT signature using JWKS keys.
			token, err := parser.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
				kid, ok := t.Header["kid"].(string)
				if !ok || kid == "" {
					return nil, fmt.Errorf("missing kid in JWT header")
				}
				return provider.GetKey(kid)
			})
			if err != nil {
				slog.Debug("auth: JWT validation failed", "error", err)
				writeAuthError(w, "invalid token")
				return
			}

			claims, ok := token.Claims.(jwt.MapClaims)
			if !ok {
				writeAuthError(w, "invalid token claims")
				return
			}

			if issuerURL != "" {
				iss, _ := claims["iss"].(string)
				if iss == "" || !IssuerMatches(iss, provider.ExpectedIssuer()) {
					slog.Debug("auth: JWT issuer mismatch", "iss", iss, "expected", provider.ExpectedIssuer())
					writeAuthError(w, "invalid token")
					return
				}
			}

			user := extractUser(claims)
			ctx := context.WithValue(r.Context(), UserContextKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// extractUser builds a User from JWT MapClaims.
func extractUser(claims jwt.MapClaims) *User {
	u := &User{}

	if sub, ok := claims["sub"].(string); ok {
		u.ID = sub
	}
	if email, ok := claims["email"].(string); ok {
		u.Email = email
	}
	if name, ok := claims["name"].(string); ok {
		u.Name = name
	}
	if tid, ok := claims["tenant_id"].(string); ok {
		u.TenantID = tid
	}

	// Roles can be []string or []interface{} depending on IdP.
	switch r := claims["roles"].(type) {
	case []interface{}:
		for _, v := range r {
			if s, ok := v.(string); ok {
				u.Roles = append(u.Roles, s)
			}
		}
	case []string:
		u.Roles = r
	}

	return u
}

func extractBearer(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

func writeAuthError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = fmt.Fprintf(w, `{"error":"%s"}`, msg)
}

// OIDCHTTPClient returns the HTTP client for provider calls, with SPKI pinning
// when configured.
func OIDCHTTPClient(cfg Config) *http.Client {
	if cfg.OIDCCertPin == "" {
		return nil
	}
	hashes := []string{cfg.OIDCCertPin}
	if cfg.OIDCCertPinBackup != "" {
		hashes = append(hashes, cfg.OIDCCertPinBackup)
	}
	pin := tlspin.NewPin(hashes...)
	slog.Info("auth: OIDC cert pinning enabled", "pins", len(hashes))
	return &http.Client{Transport: tlspin.PinnedTransport(pin), Timeout: 10 * time.Second}
}

// sessionOrProviderMiddleware accepts, in order: the legacy static token, a
// Hub session token (HS256, issued after an OIDC login or a local login),
// or a provider-signed bearer whose subject is known to the users table.
func sessionOrProviderMiddleware(sm *SessionManager, legacyToken string, provider *JWKSProvider, issuerURL, audience string, resolver SubjectResolver) func(http.Handler) http.Handler {
	providerMW := jwtMiddleware(provider, issuerURL, audience)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isExempt(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			if FromContext(r.Context()) != nil {
				next.ServeHTTP(w, r)
				return
			}
			provided := extractBearer(r)
			if provided == "" {
				writeAuthError(w, "missing Authorization header")
				return
			}
			if legacyToken != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(legacyToken)) == 1 {
				user := &User{ID: "token-user", Name: "API Token", Roles: []string{"admin"}, PlatformAdmin: true}
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), UserContextKey, user)))
				return
			}
			if claims, err := sm.VerifyAccessToken(provided); err == nil {
				user := &User{ID: claims.UserID, Email: claims.Email, Name: claims.Name, Roles: []string{claims.Role}, TenantID: claims.TenantID, PlatformAdmin: claims.PlatformAdmin}
				ctx := context.WithValue(r.Context(), UserContextKey, user)
				if claims.TenantID != "" {
					ctx = context.WithValue(ctx, TenantContextKey, claims.TenantID)
				}
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			if resolver == nil {
				writeAuthError(w, "invalid token")
				return
			}
			// Provider bearer: verify with the JWKS, then replace the claim-derived
			// user with the local one (role/tenant are ours, not the IdP's).
			providerMW(http.HandlerFunc(func(w2 http.ResponseWriter, r2 *http.Request) {
				claimUser := FromContext(r2.Context())
				if claimUser == nil {
					writeAuthError(w2, "invalid token")
					return
				}
				local, err := resolver.ResolveSubject(r2.Context(), provider.ExpectedIssuer(), claimUser.ID)
				if err != nil || local == nil {
					writeAuthError(w2, "unknown subject")
					return
				}
				ctx := context.WithValue(r2.Context(), UserContextKey, local)
				if local.TenantID != "" {
					ctx = context.WithValue(ctx, TenantContextKey, local.TenantID)
				}
				next.ServeHTTP(w2, r2.WithContext(ctx))
			})).ServeHTTP(w, r)
		})
	}
}

// RequirePlatformAdmin allows only users flagged as platform administrators
// (members of the IdP admin group, or the legacy static token).
func RequirePlatformAdmin() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u := FromContext(r.Context())
			if u == nil || !u.PlatformAdmin {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = fmt.Fprint(w, `{"error":"platform administrator required"}`)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
