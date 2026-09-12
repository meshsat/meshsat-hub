package takoperator

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"time"
)

// Errors a caller may want to distinguish.
var (
	// ErrWeakKey means a CSR's public key is too small or of a kind we do not
	// accept. Refusing it here is cheaper than discovering it in a field
	// deployment.
	ErrWeakKey = errors.New("takoperator: certificate request carries a weak or unsupported key")
	// ErrBadCSR means the request could not be parsed or its self-signature
	// did not verify, so the requester does not hold the private key.
	ErrBadCSR = errors.New("takoperator: certificate request is malformed or unsigned")
	// ErrWrapKey means the CA wrap key is missing or the wrong length.
	ErrWrapKey = errors.New("takoperator: CA wrap key must be 32 bytes of hex")
)

// minRSABits is what we accept from a phone. 2048 is the floor for anything
// touching TLS today.
const minRSABits = 2048

// NewTenantCA generates a certificate authority for one tenant and returns the
// certificate and key as PEM.
//
// RSA 2048, not the ECDSA P-256 this repo prefers elsewhere. The reason is
// interoperability rather than taste: these certificates end up in ATAK's
// truststore and in the p12 a phone imports, and in OpenTAKServer's own
// truststore, and that ecosystem is RSA by convention — OpenTAKServer's own
// generated CA is RSA. The argument for ECDSA in internal/bridge is certificate
// SIZE over satellite links, which does not apply to a phone on wifi.
//
// Ten years, because rotating a tenant CA means re-enrolling every phone in that
// tenant, and revocation of individual phones is handled by the Hub refusing a
// serial rather than by the CA's lifetime.
func NewTenantCA(label string) (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, minRSABits)
	if err != nil {
		return nil, nil, fmt.Errorf("takoperator: generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			// The label, never the tenant's name or slug: a CA subject travels
			// to every phone and is visible in any handshake, so it must leak
			// nothing about who the customer is.
			CommonName:   "MeshSat TAK " + label + " CA",
			Organization: []string{"MeshSat"},
		},
		NotBefore:             now.Add(-1 * time.Hour), // clock skew
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("takoperator: create CA certificate: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return certPEM, keyPEM, nil
}

// CA is a loaded tenant authority, ready to sign.
type CA struct {
	cert    *x509.Certificate
	key     *rsa.PrivateKey
	certPEM []byte
}

// LoadCA parses a tenant CA from PEM.
func LoadCA(certPEM, keyPEM []byte) (*CA, error) {
	cblock, _ := pem.Decode(certPEM)
	if cblock == nil {
		return nil, errors.New("takoperator: CA certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(cblock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("takoperator: parse CA certificate: %w", err)
	}
	kblock, _ := pem.Decode(keyPEM)
	if kblock == nil {
		return nil, errors.New("takoperator: CA key is not PEM")
	}
	key, err := parseRSAKey(kblock.Bytes)
	if err != nil {
		return nil, err
	}
	if !cert.IsCA {
		return nil, errors.New("takoperator: CA certificate is not a CA")
	}
	return &CA{cert: cert, key: key, certPEM: certPEM}, nil
}

func parseRSAKey(der []byte) (*rsa.PrivateKey, error) {
	if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return k, nil
	}
	any, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("takoperator: parse CA key: %w", err)
	}
	k, ok := any.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("takoperator: CA key is not RSA")
	}
	return k, nil
}

// CertPEM returns the CA certificate, which is public.
func (ca *CA) CertPEM() []byte { return ca.certPEM }

// Issued is a signed certificate and the facts the operator records about it.
type Issued struct {
	CertPEM  []byte
	Serial   string
	NotAfter time.Time
}

// IssueFromCSR signs a certificate request as CN=commonName.
//
// THE SUBJECT IN THE CSR IS IGNORED, and that is the whole point of this
// function. Upstream OpenTAKServer's enrollment signs whatever common name it
// is handed, so a user there can obtain a certificate naming `administrator` or
// another tenant's proxy identity. Here the caller states the username in the
// spec, the operator checks that the caller owns it, and the CSR contributes
// exactly one thing: a public key whose private half the requester has proven it
// holds.
//
// Extensions in the CSR are discarded for the same reason.
func (ca *CA) IssueFromCSR(csrPEM []byte, commonName string, validDays int) (*Issued, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		return nil, fmt.Errorf("%w: not PEM", ErrBadCSR)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadCSR, err)
	}
	// Without this check anyone could submit a CSR carrying somebody else's
	// public key and be issued a certificate for a key they do not hold.
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("%w: signature: %v", ErrBadCSR, err)
	}
	if err := checkPublicKey(csr.PublicKey); err != nil {
		return nil, err
	}
	if commonName == "" {
		return nil, errors.New("takoperator: refusing to issue a certificate with no common name")
	}
	if validDays <= 0 {
		validDays = EUDValidDays
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	notAfter := now.Add(time.Duration(validDays) * 24 * time.Hour)
	// A leaf may not outlive its issuer.
	if notAfter.After(ca.cert.NotAfter) {
		notAfter = ca.cert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   commonName,
			Organization: ca.cert.Subject.Organization,
		},
		NotBefore:   now.Add(-5 * time.Minute), // clock skew on a phone
		NotAfter:    notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("takoperator: sign certificate: %w", err)
	}
	return &Issued{
		CertPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		Serial:   serial.String(),
		NotAfter: notAfter,
	}, nil
}

// IssueServerCert mints the certificate an instance presents on its own
// listener, signed by the tenant CA so the Hub can verify it against the same
// trust anchor it verifies the tenant's phones against. The key is generated
// here and returned once, to be written straight into the instance's Secret.
func (ca *CA) IssueServerCert(commonName string, dnsNames []string, validDays int) (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, minRSABits)
	if err != nil {
		return nil, nil, fmt.Errorf("takoperator: generate server key: %w", err)
	}
	if validDays <= 0 {
		validDays = 825 // the CA outlives it; no public CA trusts this anyway
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	notAfter := now.Add(time.Duration(validDays) * 24 * time.Hour)
	if notAfter.After(ca.cert.NotAfter) {
		notAfter = ca.cert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   commonName,
			Organization: ca.cert.Subject.Organization,
		},
		NotBefore:   now.Add(-1 * time.Hour),
		NotAfter:    notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, nil, fmt.Errorf("takoperator: sign server certificate: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return certPEM, keyPEM, nil
}

func checkPublicKey(pub any) error {
	switch k := pub.(type) {
	case *rsa.PublicKey:
		if k.N.BitLen() < minRSABits {
			return fmt.Errorf("%w: RSA %d bits, minimum %d", ErrWeakKey, k.N.BitLen(), minRSABits)
		}
		return nil
	case *ecdsa.PublicKey:
		// Accepted even though we issue RSA ourselves: if a client can make an
		// ECDSA key, signing it costs nothing and P-256 is not weak.
		if k.Curve.Params().BitSize < 256 {
			return fmt.Errorf("%w: ECDSA curve %s", ErrWeakKey, k.Curve.Params().Name)
		}
		return nil
	default:
		return fmt.Errorf("%w: %T", ErrWeakKey, pub)
	}
}

func randomSerial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("takoperator: serial: %w", err)
	}
	// Zero is a legal but pointless serial; nudge it.
	return n.Add(n, big.NewInt(1)), nil
}

// --- wrapping the CA key at rest ---
//
// This cluster stores Secrets unencrypted in etcd, and Velero copies them to the
// object store. So a tenant CA key sitting in a Secret is a tenant CA key in two
// backups and on three control-plane disks. Wrapping it with a key that lives in
// OpenBao and reaches only this process means a Secret or a backup on its own is
// not enough to impersonate a customer's phones.
//
// Wire format is the one this codebase already uses everywhere else:
// [12-byte random nonce][ciphertext + 16-byte tag].

// ParseWrapKey decodes the hex wrap key the operator is given.
func ParseWrapKey(hexKey string) ([]byte, error) {
	raw, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWrapKey, err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("%w: got %d bytes", ErrWrapKey, len(raw))
	}
	return raw, nil
}

// WrapKeyMaterial encrypts plaintext with AES-256-GCM.
func WrapKeyMaterial(wrapKey, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(wrapKey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("takoperator: nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// UnwrapKeyMaterial reverses WrapKeyMaterial.
func UnwrapKeyMaterial(wrapKey, sealed []byte) ([]byte, error) {
	gcm, err := newGCM(wrapKey)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("takoperator: wrapped key is too short to contain a nonce")
	}
	nonce, ct := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	out, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		// Deliberately vague: a padding-or-tag oracle is not worth the
		// debugging convenience. The cause is almost always the wrong wrap key.
		return nil, errors.New("takoperator: wrapped CA key did not authenticate (wrong wrap key?)")
	}
	return out, nil
}

func newGCM(wrapKey []byte) (cipher.AEAD, error) {
	if len(wrapKey) != 32 {
		return nil, fmt.Errorf("%w: got %d bytes", ErrWrapKey, len(wrapKey))
	}
	block, err := aes.NewCipher(wrapKey)
	if err != nil {
		return nil, fmt.Errorf("takoperator: aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("takoperator: gcm: %w", err)
	}
	return gcm, nil
}
