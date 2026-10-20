package directory

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"sync"
	"testing"
)

type fakeStore struct {
	mu        sync.Mutex
	data      map[string]string
	fail      bool
	failKey   string                  // a write of this key fails (a crash at that step)
	failClear string                  // a write of "" to this key fails
	onCAS     func(key, value string) // runs before a compare-and-set, unlocked (an interleaving)
}

func newFakeStore() *fakeStore {
	return &fakeStore{data: map[string]string{}}
}

func (f *fakeStore) GetSystemConfig(_ context.Context, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data[key], nil
}

func (f *fakeStore) SetSystemConfig(_ context.Context, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail || key == f.failKey || (value == "" && key == f.failClear) {
		return errors.New("persist failed")
	}
	f.data[key] = value
	return nil
}

func (f *fakeStore) CompareAndSetSystemConfig(_ context.Context, key, old, value string) (bool, error) {
	f.mu.Lock()
	hook := f.onCAS
	f.mu.Unlock()
	if hook != nil {
		hook(key, value)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail || key == f.failKey || (value == "" && key == f.failClear) {
		return false, errors.New("persist failed")
	}
	if f.data[key] != old {
		return false, nil
	}
	f.data[key] = value
	return true, nil
}

func TestLoadOrCreateTrustAnchor_GeneratesAndPersists(t *testing.T) {
	s := newFakeStore()
	ta, err := LoadOrCreateTrustAnchor(context.Background(), s)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if len(ta.PublicKey()) == 0 {
		t.Fatal("public key is empty")
	}
	if s.data[systemConfigKey] == "" {
		t.Fatal("signing key was not persisted")
	}

	// Parse back to confirm PKIX DER is well-formed and ECDSA-P256.
	pub, err := x509.ParsePKIXPublicKey(ta.PublicKey())
	if err != nil {
		t.Fatalf("parse pubkey: %v", err)
	}
	ecPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("pubkey is %T, want *ecdsa.PublicKey", pub)
	}
	if ecPub.Curve.Params().Name != "P-256" {
		t.Fatalf("curve = %s, want P-256", ecPub.Curve.Params().Name)
	}
}

func TestLoadOrCreateTrustAnchor_ReusesStoredKey(t *testing.T) {
	s := newFakeStore()
	ta1, err := LoadOrCreateTrustAnchor(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	ta2, err := LoadOrCreateTrustAnchor(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ta1.PublicKey(), ta2.PublicKey()) {
		t.Fatal("second load generated a new key instead of reusing the stored one")
	}
}

func TestTrustAnchor_Sign_VerifiesWithPublicKey(t *testing.T) {
	ta, err := LoadOrCreateTrustAnchor(context.Background(), newFakeStore())
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("directory snapshot v1")
	sig, err := ta.Sign(msg)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	pub, err := x509.ParsePKIXPublicKey(ta.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(msg)
	if !ecdsa.VerifyASN1(pub.(*ecdsa.PublicKey), digest[:], sig) {
		t.Fatal("signature did not verify with exported pubkey")
	}
}

func TestLoadOrCreateTrustAnchor_NilStore(t *testing.T) {
	if _, err := LoadOrCreateTrustAnchor(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil store")
	}
}
