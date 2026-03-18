package auth

import (
	"strings"
	"testing"
)

func TestHashPassword_Format(t *testing.T) {
	hash, err := HashPassword("test-password-12345")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=") {
		t.Errorf("expected argon2id prefix, got %s", hash[:30])
	}
	parts := strings.Split(hash, "$")
	if len(parts) != 6 {
		t.Errorf("expected 6 parts, got %d", len(parts))
	}
}

func TestHashPassword_Unique(t *testing.T) {
	h1, _ := HashPassword("same-password-here!")
	h2, _ := HashPassword("same-password-here!")
	if h1 == h2 {
		t.Error("same password should produce different hashes (random salt)")
	}
}

func TestVerifyPassword_Correct(t *testing.T) {
	pw := "my-secure-password-123"
	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	ok, err := VerifyPassword(pw, hash)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Error("correct password should verify")
	}
}

func TestVerifyPassword_Wrong(t *testing.T) {
	hash, _ := HashPassword("correct-password!!")
	ok, err := VerifyPassword("wrong-password!!!", hash)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ok {
		t.Error("wrong password should not verify")
	}
}

func TestVerifyPassword_InvalidFormat(t *testing.T) {
	_, err := VerifyPassword("pw", "not-a-valid-hash")
	if err == nil {
		t.Error("expected error for invalid format")
	}
}

func TestValidatePasswordStrength(t *testing.T) {
	tests := []struct {
		pw string
		ok bool
	}{
		{"short", false},
		{"11-chars!!!", false},
		{"12-chars!!!!", true},
		{"a-very-long-secure-password", true},
	}
	for _, tt := range tests {
		err := ValidatePasswordStrength(tt.pw)
		if tt.ok && err != nil {
			t.Errorf("password %q should pass: %v", tt.pw, err)
		}
		if !tt.ok && err == nil {
			t.Errorf("password %q should fail", tt.pw)
		}
	}
}
