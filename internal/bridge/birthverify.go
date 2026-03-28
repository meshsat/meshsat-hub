// Package bridge provides bridge lifecycle management: MQTT subscriber,
// command dispatch, and certificate authority for bridge authentication.
package bridge

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
)

// BirthSignatureMode controls how the Hub handles unsigned birth messages.
const (
	// BirthSignatureModeWarn accepts unsigned births with a deprecation warning
	// and tags the bridge as unverified. This is the default (grace period).
	BirthSignatureModeWarn = "warn"

	// BirthSignatureModeEnforce rejects unsigned births entirely.
	BirthSignatureModeEnforce = "enforce"
)

// VerifyBirthSignature verifies the ECDSA-P256-SHA256 signature on a birth
// message. It validates:
//  1. The certificate decodes from base64 PEM
//  2. The certificate chains to the CA
//  3. The certificate CN matches the bridge_id
//  4. The ECDSA signature is valid over the canonical birth JSON
//
// Returns nil if valid. Returns an error describing the failure if invalid.
// If signature or certificate is empty, returns ErrBirthUnsigned.
func VerifyBirthSignature(birthJSON []byte, signature, certificate, bridgeID string, caCertPool *x509.CertPool) error {
	if signature == "" || certificate == "" {
		return ErrBirthUnsigned
	}

	// 1. Decode the certificate from base64 PEM.
	certPEMBytes, err := base64.StdEncoding.DecodeString(certificate)
	if err != nil {
		return fmt.Errorf("birth signature: failed to decode certificate base64: %w", err)
	}

	// 2. Parse the X.509 certificate.
	block, _ := pem.Decode(certPEMBytes)
	if block == nil {
		return fmt.Errorf("birth signature: failed to decode certificate PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("birth signature: failed to parse certificate: %w", err)
	}

	// 3. Verify the certificate chains to the bridge CA.
	opts := x509.VerifyOptions{
		Roots:     caCertPool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if _, err := cert.Verify(opts); err != nil {
		return fmt.Errorf("birth signature: certificate chain verification failed: %w", err)
	}

	// 4. Verify the certificate CN matches the bridge_id.
	if cert.Subject.CommonName != bridgeID {
		return fmt.Errorf("birth signature: certificate CN %q does not match bridge_id %q", cert.Subject.CommonName, bridgeID)
	}

	// 5. Extract the ECDSA public key from the certificate.
	pubKey, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("birth signature: certificate public key is not ECDSA")
	}

	// 6. Reconstruct canonical JSON (remove signature from the birth payload).
	var m map[string]interface{}
	if err := json.Unmarshal(birthJSON, &m); err != nil {
		return fmt.Errorf("birth signature: failed to unmarshal birth for verification: %w", err)
	}
	delete(m, "signature")
	canonical, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("birth signature: failed to marshal canonical birth: %w", err)
	}

	// 7. Verify the ECDSA signature.
	sigBytes, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("birth signature: failed to decode signature base64: %w", err)
	}
	hash := sha256.Sum256(canonical)
	if !ecdsa.VerifyASN1(pubKey, hash[:], sigBytes) {
		return fmt.Errorf("birth signature: ECDSA verification failed")
	}

	return nil
}

// ErrBirthUnsigned is returned when a birth message has no signature.
var ErrBirthUnsigned = fmt.Errorf("birth message is unsigned")
