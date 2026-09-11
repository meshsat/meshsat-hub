package takfront

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"
)

// testCA is a throwaway certificate authority. Tenants get one each, so a
// certificate issued by one is by construction not issuable by another.
//
// Keys are ECDSA P-256 because these tests generate dozens of them; real phones
// carry RSA keys from OpenTAKServer's CA, which crypto/tls handles identically.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"MeshSat test"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("self-sign CA: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	return &testCA{cert: cert, key: key}
}

type certOpt func(*x509.Certificate)

// expired issues a certificate whose validity ended before now.
func expired() certOpt {
	return func(c *x509.Certificate) {
		c.NotBefore = time.Now().Add(-48 * time.Hour)
		c.NotAfter = time.Now().Add(-24 * time.Hour)
	}
}

// noCommonName strips the subject, as a certificate issued from a CSR with only
// SANs would.
func noCommonName() certOpt {
	return func(c *x509.Certificate) { c.Subject = pkix.Name{} }
}

// forDNS makes the certificate usable as a server certificate for host.
func forDNS(host string) certOpt {
	return func(c *x509.Certificate) {
		c.DNSNames = append(c.DNSNames, host)
		c.IPAddresses = append(c.IPAddresses, net.ParseIP("127.0.0.1"))
	}
}

// serverOnly removes client authentication, so the certificate cannot be used
// by a phone.
func serverOnly() certOpt {
	return func(c *x509.Certificate) {
		c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
}

func (ca *testCA) issue(t *testing.T, cn string, opts ...certOpt) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key for %s: %v", cn, err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth,
		},
	}
	for _, opt := range opts {
		opt(tmpl)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("issue %s: %v", cn, err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse %s: %v", cn, err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

func (ca *testCA) pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return pool
}

func serial(t *testing.T) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 96))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	return n.Add(n, big.NewInt(1))
}

// rawChain is a tls.Certificate as VerifyPeerCertificate sees it.
func rawChain(c tls.Certificate) [][]byte { return c.Certificate }
