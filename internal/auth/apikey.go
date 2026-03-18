package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	// APIKeyPrefix is prepended to all generated API keys.
	APIKeyPrefix = "meshsat_"
	// apiKeyBytes is the number of random bytes (32 → 64 hex chars).
	apiKeyBytes = 32
)

// APIKeyValidator looks up an API key by its SHA-256 hash and returns
// the associated user info and tenant ID. Returns an error if not found.
type APIKeyValidator func(ctx context.Context, keyHash string) (*User, string, error)

// GenerateAPIKey creates a new random API key with the meshsat_ prefix.
// Returns (plaintext key, SHA-256 hash of the full key, display prefix).
func GenerateAPIKey() (plaintext, hash, prefix string, err error) {
	b := make([]byte, apiKeyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", "", fmt.Errorf("generate api key: %w", err)
	}
	raw := hex.EncodeToString(b) // 64 hex chars
	plaintext = APIKeyPrefix + raw
	hash = HashAPIKey(plaintext)
	prefix = plaintext[:len(APIKeyPrefix)+8] // "meshsat_" + first 8 hex chars
	return plaintext, hash, prefix, nil
}

// HashAPIKey returns the SHA-256 hex digest of an API key.
func HashAPIKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// IsAPIKey returns true if the token starts with the meshsat_ prefix.
func IsAPIKey(token string) bool {
	return strings.HasPrefix(token, APIKeyPrefix)
}

// APIKeyMiddleware returns middleware that authenticates requests bearing
// a meshsat_ API key. If the bearer token is not an API key, it passes
// through to the next middleware (allowing JWT/token auth to handle it).
func APIKeyMiddleware(validate APIKeyValidator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isExempt(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			token := extractBearer(r)
			if token == "" || !IsAPIKey(token) {
				// Not an API key — let subsequent auth middleware handle it.
				next.ServeHTTP(w, r)
				return
			}

			hash := HashAPIKey(token)
			user, tenantID, err := validate(r.Context(), hash)
			if err != nil {
				slog.Debug("auth: API key validation failed", "error", err)
				writeAuthError(w, "invalid API key")
				return
			}

			// Check expiry.
			if !user.ExpiresAt.IsZero() && time.Now().After(user.ExpiresAt) {
				writeAuthError(w, "API key expired")
				return
			}

			ctx := context.WithValue(r.Context(), UserContextKey, user)
			if tenantID != "" {
				ctx = context.WithValue(ctx, TenantContextKey, tenantID)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
