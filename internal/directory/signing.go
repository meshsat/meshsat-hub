package directory

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"sync"
)

// systemConfigKey is the system_config key under which the Hub's
// ECDSA-P256 directory-signing private key is persisted as PEM.
const systemConfigKey = "directory_signing_key"

// signingKeyStore is the subset of store.Store used by TrustAnchor.
// It is declared here to avoid an import cycle with internal/store.
type signingKeyStore interface {
	GetSystemConfig(ctx context.Context, key string) (string, error)
	SetSystemConfig(ctx context.Context, key, value string) error
}

// TrustAnchor holds the Hub's ECDSA-P256 directory-signing keypair.
// The private key signs directory snapshots; bridges pin the public key
// on first provision (MESHSAT-539) and verify snapshots offline.
type TrustAnchor struct {
	mu     sync.RWMutex
	key    *ecdsa.PrivateKey
	pub    []byte // PKIX DER
	pem    string // the stored form, to tell a changed key (Refresh)
	pemSum [32]byte
}

// current returns the stored form of the current key and its id.
func (t *TrustAnchor) current() (string, string) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.pem, KeyID(t.pub)
}

// set makes key current.
func (t *TrustAnchor) set(key *ecdsa.PrivateKey, pub []byte, pemStr string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.key, t.pub, t.pem, t.pemSum = key, pub, pemStr, sha256.Sum256([]byte(pemStr))
}

// sealWith seals with the current key only if it is keyID: a rotation's
// records must be signed by the key they rotate away from, whatever a
// refresh did meanwhile.
func (t *TrustAnchor) sealWith(keyID string, payload []byte) (*Envelope, error) {
	env, err := t.Seal(payload)
	if err != nil {
		return nil, err
	}
	if env.KeyID != keyID {
		return nil, fmt.Errorf("directory: the current key is %s, not %s", env.KeyID, keyID)
	}
	return env, nil
}

// LoadOrCreateTrustAnchor returns the TrustAnchor from s, generating and
// persisting a new ECDSA-P256 keypair if none is stored yet.
func LoadOrCreateTrustAnchor(ctx context.Context, s signingKeyStore) (*TrustAnchor, error) {
	if s == nil {
		return nil, errors.New("directory: signing key store is nil")
	}
	stored, _ := s.GetSystemConfig(ctx, systemConfigKey)
	if stored != "" {
		key, err := parseECPrivateKeyPEM([]byte(stored))
		if err == nil {
			pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
			if err != nil {
				return nil, fmt.Errorf("directory: marshal stored pubkey: %w", err)
			}
			t := &TrustAnchor{}
			t.set(key, pub, stored)
			return t, nil
		}
	}

	key, pemBytes, err := generateECPrivateKeyPEM()
	if err != nil {
		return nil, err
	}
	if err := s.SetSystemConfig(ctx, systemConfigKey, string(pemBytes)); err != nil {
		return nil, fmt.Errorf("directory: persist signing key: %w", err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("directory: marshal generated pubkey: %w", err)
	}
	t := &TrustAnchor{}
	t.set(key, pub, string(pemBytes))
	return t, nil
}

// PublicKey returns the ECDSA-P256 public key in PKIX DER encoding.
// Bridges parse with x509.ParsePKIXPublicKey.
func (t *TrustAnchor) PublicKey() []byte {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]byte, len(t.pub))
	copy(out, t.pub)
	return out
}

// Sign returns an ASN.1 DER ECDSA signature over SHA-256(msg).
func (t *TrustAnchor) Sign(msg []byte) ([]byte, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	digest := sha256.Sum256(msg)
	return ecdsa.SignASN1(rand.Reader, t.key, digest[:])
}

func generateECPrivateKeyPEM() (*ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("directory: generate signing key: %w", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("directory: marshal signing key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	return key, pemBytes, nil
}

func parseECPrivateKeyPEM(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("directory: invalid PEM")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}
