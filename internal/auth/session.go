package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// AccessTokenTTL is the lifetime of an access token (short-lived).
	AccessTokenTTL = 15 * time.Minute
	// RefreshTokenTTL is the lifetime of a refresh token (long-lived, stored hashed in DB).
	RefreshTokenTTL = 7 * 24 * time.Hour
	// refreshTokenBytes is the entropy for refresh tokens.
	refreshTokenBytes = 32
)

// SessionClaims are the JWT claims for access tokens.
type SessionClaims struct {
	jwt.RegisteredClaims
	UserID   string `json:"uid"`
	Email    string `json:"email,omitempty"`
	Name     string `json:"name,omitempty"`
	Role     string `json:"role"`
	TenantID string `json:"tid,omitempty"`
}

// SessionManager handles JWT access token signing/verification and refresh token generation.
type SessionManager struct {
	signingKey []byte
	issuer     string
}

// NewSessionManager creates a session manager with the given HMAC-SHA256 signing key.
// The key must be at least 32 bytes. If empty, a random key is generated (tokens
// will not survive restarts — acceptable for standalone mode).
func NewSessionManager(key []byte, issuer string) *SessionManager {
	if len(key) < 32 {
		key = make([]byte, 32)
		_, _ = rand.Read(key)
	}
	if issuer == "" {
		issuer = "meshsat-hub"
	}
	return &SessionManager{signingKey: key, issuer: issuer}
}

// IssueAccessToken creates a signed JWT access token for the given user.
func (sm *SessionManager) IssueAccessToken(userID, email, name, role, tenantID string) (string, error) {
	now := time.Now()
	claims := SessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    sm.issuer,
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(AccessTokenTTL)),
		},
		UserID:   userID,
		Email:    email,
		Name:     name,
		Role:     role,
		TenantID: tenantID,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(sm.signingKey)
}

// VerifyAccessToken parses and validates a JWT access token.
func (sm *SessionManager) VerifyAccessToken(tokenStr string) (*SessionClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &SessionClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return sm.signingKey, nil
	}, jwt.WithIssuer(sm.issuer), jwt.WithExpirationRequired())
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}

	claims, ok := token.Claims.(*SessionClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid claims")
	}

	return claims, nil
}

// GenerateRefreshToken creates a cryptographically random refresh token.
// Returns (plaintext, SHA-256 hash for DB storage).
func GenerateRefreshToken() (plaintext, hash string, err error) {
	b := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate refresh token: %w", err)
	}
	plaintext = hex.EncodeToString(b)
	hash = HashRefreshToken(plaintext)
	return plaintext, hash, nil
}

// HashRefreshToken returns the SHA-256 hex digest of a refresh token.
func HashRefreshToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
