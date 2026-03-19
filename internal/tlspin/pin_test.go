package tlspin

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

func selfSignedCert(t *testing.T) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(derBytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestSPKIHash(t *testing.T) {
	cert := selfSignedCert(t)
	hash := SPKIHash(cert)
	if hash == "" {
		t.Error("expected non-empty hash")
	}
	if len(hash) != 44 { // base64-encoded SHA-256 = 44 chars
		t.Errorf("hash length = %d, want 44", len(hash))
	}

	// Same cert should produce same hash.
	if SPKIHash(cert) != hash {
		t.Error("same cert should produce same hash")
	}
}

func TestVerify_Match(t *testing.T) {
	cert := selfSignedCert(t)
	hash := SPKIHash(cert)
	pin := NewPin(hash)

	chains := [][]*x509.Certificate{{cert}}
	if err := pin.Verify(chains); err != nil {
		t.Errorf("expected match: %v", err)
	}
}

func TestVerify_NoMatch(t *testing.T) {
	cert := selfSignedCert(t)
	pin := NewPin("dGhpcyBpcyBub3QgYSByZWFsIGhhc2ggYXQgYWxs") // fake hash

	chains := [][]*x509.Certificate{{cert}}
	if err := pin.Verify(chains); err == nil {
		t.Error("expected no match error")
	}
}

func TestVerify_BackupPin(t *testing.T) {
	cert := selfSignedCert(t)
	hash := SPKIHash(cert)
	pin := NewPin("fakehash1234567890", hash) // primary fake, backup matches

	chains := [][]*x509.Certificate{{cert}}
	if err := pin.Verify(chains); err != nil {
		t.Errorf("backup pin should match: %v", err)
	}
}

func TestVerify_NoPins(t *testing.T) {
	cert := selfSignedCert(t)
	pin := NewPin()

	chains := [][]*x509.Certificate{{cert}}
	if err := pin.Verify(chains); err != nil {
		t.Errorf("no pins should allow all: %v", err)
	}
}

func TestPinnedClient(t *testing.T) {
	pin := NewPin("somehash")
	client := PinnedClient(pin)
	if client == nil {
		t.Fatal("expected non-nil client")
	}
	if client.Transport == nil {
		t.Fatal("expected non-nil transport")
	}
}
