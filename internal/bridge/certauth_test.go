package bridge

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

func TestNewSelfSignedCA(t *testing.T) {
	ca, certPEM, keyPEM, err := NewSelfSignedCA("MeshSat Test")
	if err != nil {
		t.Fatalf("NewSelfSignedCA: %v", err)
	}
	if ca == nil {
		t.Fatal("expected non-nil CA")
	}
	if len(certPEM) == 0 {
		t.Fatal("expected non-empty cert PEM")
	}
	if len(keyPEM) == 0 {
		t.Fatal("expected non-empty key PEM")
	}
	if ca.caCert.Subject.CommonName != "MeshSat Test Bridge CA" {
		t.Errorf("unexpected CN: %s", ca.caCert.Subject.CommonName)
	}
	if !ca.caCert.IsCA {
		t.Error("expected CA flag to be set")
	}
}

func TestNewCertAuthority(t *testing.T) {
	_, certPEM, keyPEM, err := NewSelfSignedCA("Test")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	ca, err := NewCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("NewCertAuthority: %v", err)
	}
	if ca == nil {
		t.Fatal("expected non-nil CA")
	}
}

func TestNewCertAuthority_InvalidCert(t *testing.T) {
	_, err := NewCertAuthority([]byte("not a PEM"), []byte("not a PEM"))
	if err == nil {
		t.Fatal("expected error for invalid PEM")
	}
}

func TestIssueBridgeCert(t *testing.T) {
	ca, _, _, err := NewSelfSignedCA("MeshSat")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	certPEM, keyPEM, err := ca.IssueBridgeCert("mule01", 90)
	if err != nil {
		t.Fatalf("IssueBridgeCert: %v", err)
	}

	// Verify cert PEM is valid.
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("failed to decode cert PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	if cert.Subject.CommonName != "mule01" {
		t.Errorf("expected CN=mule01, got %s", cert.Subject.CommonName)
	}
	if len(cert.ExtKeyUsage) == 0 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Error("expected ClientAuth extended key usage")
	}

	// Verify key PEM is valid.
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		t.Fatal("failed to decode key PEM")
	}
	_, err = x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatalf("parse key: %v", err)
	}

	// Cert should expire ~90 days from now.
	expectedExpiry := time.Now().Add(90 * 24 * time.Hour)
	diff := cert.NotAfter.Sub(expectedExpiry)
	if diff < -1*time.Hour || diff > 1*time.Hour {
		t.Errorf("unexpected expiry: %v (expected ~%v)", cert.NotAfter, expectedExpiry)
	}
}

func TestIssueBridgeCert_DefaultDays(t *testing.T) {
	ca, _, _, err := NewSelfSignedCA("MeshSat")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	certPEM, _, err := ca.IssueBridgeCert("test-bridge", 0)
	if err != nil {
		t.Fatalf("IssueBridgeCert: %v", err)
	}

	block, _ := pem.Decode(certPEM)
	cert, _ := x509.ParseCertificate(block.Bytes)
	expectedExpiry := time.Now().Add(90 * 24 * time.Hour)
	diff := cert.NotAfter.Sub(expectedExpiry)
	if diff < -1*time.Hour || diff > 1*time.Hour {
		t.Errorf("default days should be 90, got expiry: %v", cert.NotAfter)
	}
}

func TestVerifyBridgeCert(t *testing.T) {
	ca, _, _, err := NewSelfSignedCA("MeshSat")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	certPEM, _, err := ca.IssueBridgeCert("bananapi01", 30)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	bridgeID, err := ca.VerifyBridgeCert(certPEM)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if bridgeID != "bananapi01" {
		t.Errorf("expected bridgeID=bananapi01, got %s", bridgeID)
	}
}

func TestVerifyBridgeCert_DifferentCA(t *testing.T) {
	ca1, _, _, err := NewSelfSignedCA("CA One")
	if err != nil {
		t.Fatalf("setup CA1: %v", err)
	}
	ca2, _, _, err := NewSelfSignedCA("CA Two")
	if err != nil {
		t.Fatalf("setup CA2: %v", err)
	}

	// Issue cert from CA1, try to verify with CA2.
	certPEM, _, err := ca1.IssueBridgeCert("rogue-bridge", 30)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	_, err = ca2.VerifyBridgeCert(certPEM)
	if err == nil {
		t.Fatal("expected verification failure for cert from different CA")
	}
}

func TestVerifyBridgeCert_Expired(t *testing.T) {
	// Create a CA manually and issue an already-expired cert.
	ca, _, _, err := NewSelfSignedCA("MeshSat")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Generate a client cert that expired yesterday.
	clientKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serialNumber, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject:      pkix.Name{CommonName: "expired-bridge"},
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     time.Now().Add(-24 * time.Hour), // expired
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, ca.caCert, &clientKey.PublicKey, ca.caKey)
	if err != nil {
		t.Fatalf("create expired cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	_, err = ca.VerifyBridgeCert(certPEM)
	if err == nil {
		t.Fatal("expected verification failure for expired certificate")
	}
}

func TestVerifyBridgeCert_InvalidPEM(t *testing.T) {
	ca, _, _, _ := NewSelfSignedCA("MeshSat")
	_, err := ca.VerifyBridgeCert([]byte("not a certificate"))
	if err == nil {
		t.Fatal("expected error for invalid PEM")
	}
}

func TestCACertPEM(t *testing.T) {
	ca, certPEM, _, err := NewSelfSignedCA("MeshSat")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if string(ca.CACertPEM()) != string(certPEM) {
		t.Error("CACertPEM() should return the same PEM passed at construction")
	}
}
