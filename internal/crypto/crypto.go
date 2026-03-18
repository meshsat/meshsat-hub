// Package crypto provides AES-256-GCM encryption/decryption and per-device
// key management for end-to-end encrypted satellite messaging.
//
// Wire format: [12-byte random nonce][ciphertext + 16-byte GCM auth tag]
// This is identical across meshsat (Go), meshsat-android (Kotlin), and meshsat-hub (Go).
//
// Key management modes:
//   - Server-side decrypt: Hub stores the key and can decrypt messages
//   - Pass-through (true E2E): Hub stores the key hash but cannot decrypt;
//     messages pass through opaque
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

const (
	KeySize   = 32                  // AES-256
	NonceSize = 12                  // GCM standard nonce
	TagSize   = 16                  // GCM auth tag
	Overhead  = NonceSize + TagSize // 28 bytes overhead per encrypted message
)

// GenerateKey creates a new random AES-256 key.
func GenerateKey() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("crypto: generate key: %w", err)
	}
	return key, nil
}

// KeyHash returns the SHA-256 hash of a key (for identification without exposing the key).
func KeyHash(key []byte) string {
	h := sha256.Sum256(key)
	return hex.EncodeToString(h[:])
}

// Encrypt encrypts plaintext with AES-256-GCM.
// Returns: [12-byte nonce][ciphertext + 16-byte tag]
func Encrypt(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: new gcm: %w", err)
	}

	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("crypto: random nonce: %w", err)
	}

	// Seal appends ciphertext+tag to nonce
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt decrypts an AES-256-GCM ciphertext.
// Input format: [12-byte nonce][ciphertext + 16-byte tag]
func Decrypt(key, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < Overhead {
		return nil, fmt.Errorf("crypto: ciphertext too short (%d bytes, min %d)", len(ciphertext), Overhead)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: new gcm: %w", err)
	}

	nonce := ciphertext[:NonceSize]
	sealed := ciphertext[NonceSize:]

	plaintext, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: decrypt: %w", err)
	}
	return plaintext, nil
}
