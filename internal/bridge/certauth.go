// Package bridge provides bridge lifecycle management: MQTT subscriber,
// command dispatch, and certificate authority for bridge authentication.
package bridge

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// CertAuthority issues and verifies TLS client certificates for field bridges.
// Uses ECDSA P-256 for small certificate size (important for satellite links).
type CertAuthority struct {
	caCert    *x509.Certificate
	caKey     *ecdsa.PrivateKey
	caCertPEM []byte
}

// NewCertAuthority loads a CA from PEM-encoded certificate and key.
func NewCertAuthority(caCertPEM, caKeyPEM []byte) (*CertAuthority, error) {
	block, _ := pem.Decode(caCertPEM)
	if block == nil {
		return nil, fmt.Errorf("certauth: failed to decode CA certificate PEM")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("certauth: parse CA cert: %w", err)
	}

	keyBlock, _ := pem.Decode(caKeyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("certauth: failed to decode CA key PEM")
	}
	caKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("certauth: parse CA key: %w", err)
	}

	return &CertAuthority{
		caCert:    caCert,
		caKey:     caKey,
		caCertPEM: caCertPEM,
	}, nil
}

// NewSelfSignedCA generates a new self-signed CA for bridge certificate issuance.
func NewSelfSignedCA(org string) (*CertAuthority, []byte, []byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("certauth: generate CA key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("certauth: generate serial: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{org},
			CommonName:   org + " Bridge CA",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour), // 10 years
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("certauth: create CA cert: %w", err)
	}

	caCert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("certauth: parse created CA cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyDER, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("certauth: marshal CA key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	ca := &CertAuthority{
		caCert:    caCert,
		caKey:     caKey,
		caCertPEM: certPEM,
	}
	return ca, certPEM, keyPEM, nil
}

// CACertPEM returns the PEM-encoded CA certificate.
func (ca *CertAuthority) CACertPEM() []byte {
	return ca.caCertPEM
}

// IssueBridgeCert generates an x509 client certificate for the given bridge.
// The certificate CN is set to the bridgeID. validDays controls the expiry
// (default 90 if <= 0). Returns PEM-encoded cert and key.
// The private key is returned once and NEVER stored server-side.
func (ca *CertAuthority) IssueBridgeCert(bridgeID string, validDays int) (certPEM, keyPEM []byte, err error) {
	if validDays <= 0 {
		validDays = 90
	}

	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("certauth: generate client key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("certauth: generate serial: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   bridgeID,
			Organization: []string{ca.caCert.Subject.Organization[0]},
		},
		NotBefore: time.Now().Add(-5 * time.Minute), // slight clock skew tolerance
		NotAfter:  time.Now().Add(time.Duration(validDays) * 24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth,
		},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, ca.caCert, &clientKey.PublicKey, ca.caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("certauth: create client cert: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyDER, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		return nil, nil, fmt.Errorf("certauth: marshal client key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return certPEM, keyPEM, nil
}

// VerifyBridgeCert verifies that a certificate was issued by this CA and returns
// the bridge ID (CN) from the certificate subject.
func (ca *CertAuthority) VerifyBridgeCert(certPEM []byte) (bridgeID string, err error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", fmt.Errorf("certauth: failed to decode certificate PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("certauth: parse certificate: %w", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca.caCert)

	opts := x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	if _, err := cert.Verify(opts); err != nil {
		return "", fmt.Errorf("certauth: verification failed: %w", err)
	}

	return cert.Subject.CommonName, nil
}
