// Package auth provides authentication middleware for the Hub API.
// Supports three modes:
// - "none": no authentication (development only)
// - "token": simple bearer token (standalone, HUB_AUTH_TOKEN)
// - "oidc": JWT validation against an OIDC provider (cluster/k8s)
package auth

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type contextKey string

const UserContextKey contextKey = "auth_user"

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
		return jwtMiddleware(cfg.OIDCIssuerURL, cfg.OIDCAudience)
	case "token":
		slog.Info("auth: token mode")
		return tokenMiddleware(cfg.Token)
	default:
		slog.Warn("auth: no authentication (mode=none)")
		return noopMiddleware()
	}
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

func jwtMiddleware(issuerURL, audience string) func(http.Handler) http.Handler {
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

			user, err := parseJWT(tokenStr, issuerURL, audience)
			if err != nil {
				slog.Debug("auth: JWT validation failed", "error", err)
				writeAuthError(w, "invalid token")
				return
			}

			ctx := context.WithValue(r.Context(), UserContextKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// parseJWT does lightweight JWT claim extraction and validation.
// Does NOT verify the signature (signature verification requires JWKS fetching
// from the IdP — a full implementation should use github.com/golang-jwt/jwt/v5).
// For production, replace this with proper JWKS-based verification.
func parseJWT(tokenStr, issuerURL, audience string) (*User, error) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid JWT format")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}

	var claims struct {
		Sub      string      `json:"sub"`
		Email    string      `json:"email"`
		Name     string      `json:"name"`
		Iss      string      `json:"iss"`
		Aud      interface{} `json:"aud"`
		Exp      int64       `json:"exp"`
		Roles    []string    `json:"roles"`
		TenantID string      `json:"tenant_id"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("parse claims: %w", err)
	}

	if claims.Exp > 0 && time.Now().Unix() > claims.Exp {
		return nil, fmt.Errorf("token expired")
	}

	if issuerURL != "" && claims.Iss != issuerURL {
		return nil, fmt.Errorf("issuer mismatch")
	}

	if audience != "" && !audienceContains(claims.Aud, audience) {
		return nil, fmt.Errorf("audience mismatch")
	}

	return &User{
		ID:       claims.Sub,
		Email:    claims.Email,
		Name:     claims.Name,
		Roles:    claims.Roles,
		TenantID: claims.TenantID,
	}, nil
}

func extractBearer(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

func audienceContains(aud interface{}, target string) bool {
	switch v := aud.(type) {
	case string:
		return v == target
	case []interface{}:
		for _, a := range v {
			if s, ok := a.(string); ok && s == target {
				return true
			}
		}
	}
	return false
}

func writeAuthError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	fmt.Fprintf(w, `{"error":"%s"}`, msg)
}
