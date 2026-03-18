package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestSessionManager_IssueAndVerify(t *testing.T) {
	sm := NewSessionManager([]byte("test-secret-key-32bytes-minimum!"), "test-issuer")

	token, err := sm.IssueAccessToken("user-1", "test@example.com", "Test User", "owner", "tenant-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if token == "" {
		t.Fatal("token should not be empty")
	}

	claims, err := sm.VerifyAccessToken(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.UserID != "user-1" {
		t.Errorf("expected user-1, got %s", claims.UserID)
	}
	if claims.Email != "test@example.com" {
		t.Errorf("expected test@example.com, got %s", claims.Email)
	}
	if claims.Role != "owner" {
		t.Errorf("expected owner, got %s", claims.Role)
	}
	if claims.TenantID != "tenant-1" {
		t.Errorf("expected tenant-1, got %s", claims.TenantID)
	}
}

func TestSessionManager_ExpiredToken(t *testing.T) {
	key := []byte("test-secret-key-32bytes-minimum!")
	sm := &SessionManager{signingKey: key, issuer: "test"}

	// Manually create an expired token
	claims := SessionClaims{}
	claims.Issuer = "test"
	claims.Subject = "user-1"
	claims.IssuedAt = jwt.NewNumericDate(time.Now().Add(-1 * time.Hour))
	claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-30 * time.Minute))
	claims.UserID = "user-1"
	claims.Role = "viewer"

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, _ := token.SignedString(key)

	_, err := sm.VerifyAccessToken(tokenStr)
	if err == nil {
		t.Error("expired token should fail verification")
	}
}

func TestSessionManager_WrongKey(t *testing.T) {
	sm1 := NewSessionManager([]byte("key-one-is-32bytes-or-more-here!"), "test")
	sm2 := NewSessionManager([]byte("key-two-is-different-from-above!"), "test")

	token, _ := sm1.IssueAccessToken("user-1", "", "", "viewer", "")
	_, err := sm2.VerifyAccessToken(token)
	if err == nil {
		t.Error("wrong key should fail verification")
	}
}

func TestSessionManager_RandomKeyGeneration(t *testing.T) {
	sm := NewSessionManager(nil, "")
	token, err := sm.IssueAccessToken("u", "", "", "viewer", "")
	if err != nil {
		t.Fatalf("issue with random key: %v", err)
	}
	claims, err := sm.VerifyAccessToken(token)
	if err != nil {
		t.Fatalf("verify with random key: %v", err)
	}
	if claims.UserID != "u" {
		t.Errorf("expected u, got %s", claims.UserID)
	}
}

func TestGenerateRefreshToken(t *testing.T) {
	plain, hash, err := GenerateRefreshToken()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(plain) != 64 { // 32 bytes hex-encoded
		t.Errorf("expected 64 char plaintext, got %d", len(plain))
	}
	if len(hash) != 64 { // SHA-256 hex
		t.Errorf("expected 64 char hash, got %d", len(hash))
	}
	// Verify hash matches
	if HashRefreshToken(plain) != hash {
		t.Error("hash should match")
	}
}

func TestGenerateRefreshToken_Unique(t *testing.T) {
	p1, _, _ := GenerateRefreshToken()
	p2, _, _ := GenerateRefreshToken()
	if p1 == p2 {
		t.Error("refresh tokens should be unique")
	}
}
