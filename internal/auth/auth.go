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

	"github.com/golang-jwt/jwt/v5"
)

type contextKey string

const UserContextKey contextKey = "auth_user"
const TenantContextKey contextKey = "tenant_id"

// User represents an authenticated user or API client.
type User struct {
	ID       string   `json:"id"`
	Email    string   `json:"email,omitempty"`
	Name     string   `json:"name,omitempty"`
	Roles    []string `json:"roles,omitempty"`
	TenantID string   `json:"tenant_id,omitempty"`
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
//  1. User.TenantID from JWT "tenant_id" claim (set by auth middleware)
//  2. X-Tenant-ID header (for service-to-service calls)
//  3. Falls back to "default" (single-tenant compatibility)
//
// If enforce is true, requests without a resolvable tenant get a 403 response.
func TenantMiddleware(enforce bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip tenant resolution for exempt paths.
			if isExempt(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			var tenantID string

			// 1. From authenticated user (JWT claim).
			if u := FromContext(r.Context()); u != nil && u.TenantID != "" {
				tenantID = u.TenantID
			}

			// 2. From X-Tenant-ID header (service-to-service).
			if tenantID == "" {
				tenantID = r.Header.Get("X-Tenant-ID")
			}

			// 3. Default fallback.
			if tenantID == "" {
				if enforce {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					_, _ = fmt.Fprintf(w, `{"error":"tenant context required"}`)
					return
				}
				tenantID = "default"
			}

			ctx := context.WithValue(r.Context(), TenantContextKey, tenantID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Config holds authentication configuration.
type Config struct {
	Mode          string // "none", "token", "oidc"
	Token         string // static bearer token (mode=token)
	OIDCIssuerURL string // OIDC issuer URL (mode=oidc)
	OIDCAudience  string // expected JWT audience
}

// Middleware returns an HTTP middleware that authenticates requests.
func Middleware(cfg Config) func(http.Handler) http.Handler {
	switch cfg.Mode {
	case "oidc":
		slog.Info("auth: OIDC mode", "issuer", cfg.OIDCIssuerURL)
		provider := NewJWKSProvider(cfg.OIDCIssuerURL, nil)
		return jwtMiddleware(provider, cfg.OIDCIssuerURL, cfg.OIDCAudience)
	case "token":
		slog.Info("auth: token mode")
		return tokenMiddleware(cfg.Token)
	default:
		slog.Warn("auth: no authentication (mode=none)")
		return noopMiddleware()
	}
}

// MiddlewareWithProvider returns OIDC middleware using an externally provided JWKSProvider.
// This is useful for testing and for sharing a provider across components.
func MiddlewareWithProvider(provider *JWKSProvider, issuerURL, audience string) func(http.Handler) http.Handler {
	return jwtMiddleware(provider, issuerURL, audience)
}

func isExempt(path string) bool {
	return path == "/healthz" || path == "/readyz" ||
		path == "/api/webhook/rockblock" ||
		!strings.HasPrefix(path, "/api/")
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

			provided := extractBearer(r)
			if provided == "" {
				writeAuthError(w, "missing Authorization header")
				return
			}
			if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
				writeAuthError(w, "invalid token")
				return
			}

			user := &User{ID: "token-user", Name: "API Token", Roles: []string{"admin"}}
			ctx := context.WithValue(r.Context(), UserContextKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func jwtMiddleware(provider *JWKSProvider, issuerURL, audience string) func(http.Handler) http.Handler {
	// Build parser options.
	opts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512"}),
		jwt.WithExpirationRequired(),
	}
	if issuerURL != "" {
		opts = append(opts, jwt.WithIssuer(issuerURL))
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
