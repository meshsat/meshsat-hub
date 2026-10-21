package codec

import (
	"context"
	"encoding/base64"

	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
)

// Declaration is what the Hub knows of a modem's satellite payload format,
// from the bridge that owns it (HeMB-NG S3-2).
type Declaration int

const (
	// Undeclared: no bridge declared a format for this modem (an older
	// bridge, a phone, a device no bridge owns). Read as before.
	Undeclared Declaration = iota
	// DeclaredSealed: the owning bridge's send chain encrypts, so a versioned
	// payload the Hub cannot decrypt is ciphertext for the bridges' key.
	DeclaredSealed
	// DeclaredPlain: the owning bridge declared a chain without encryption.
	DeclaredPlain
	// DeclarationUnavailable: the lookup failed. The modem may be a sealing
	// one, so its binary is never guessed at.
	DeclarationUnavailable
)

func (d Declaration) String() string {
	switch d {
	case DeclaredSealed:
		return "sealed"
	case DeclaredPlain:
		return "plain"
	case DeclarationUnavailable:
		return "unavailable"
	}
	return "undeclared"
}

// EgressDeclarations answers what the bridge that owns a modem declared
// about its satellite payloads. Only a declaration tells ciphertext from
// plain SMAZ2: both are binary after the version byte, and a guess by length
// or printability gets one of them wrong.
type EgressDeclarations interface {
	Declaration(ctx context.Context, tenantID, deviceID string) Declaration
}

// SealedForAnother reports whether a payload the Hub could not decrypt is
// ciphertext for a key it does not hold, which the Hub relays but never reads
// as text. The Bridge puts the version byte in front only after its egress
// transforms, so versioned means a chain ran, not that it encrypted. It is
// sealed when:
//   - it is base64 of binary at least a nonce and tag long: an encrypting
//     chain that ends in base64 (the kits' smaz2, encrypt, base64), as the
//     Cloudloop path has always read it;
//   - or it is longer than a nonce and tag and the owning bridge declared the
//     chain sealing (an encrypting chain without base64, HeMB-NG S3-2), or
//     the declaration could not be read: a modem that may be sealing is not
//     guessed at.
//
// Undeclared or declared plain, binary after the version byte is read as
// before: a plain SMAZ2 chain's output is binary too.
func SealedForAnother(protoVersion byte, decrypted bool, b []byte, d Declaration) bool {
	if protoVersion == 0 || decrypted {
		return false
	}
	if IsBase64Ciphertext(b) {
		return true
	}
	switch d {
	case DeclaredSealed, DeclarationUnavailable:
		return len(b) > hubcrypto.Overhead
	}
	return false
}

// IsBase64Ciphertext reports whether b is base64 text whose decoded form is
// binary and at least as long as an AES-GCM nonce plus tag: the tail of the
// Bridge's smaz2 -> encrypt -> base64 egress chain.
func IsBase64Ciphertext(b []byte) bool {
	if len(b) < 40 {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(string(b))
	if err != nil || len(raw) < hubcrypto.Overhead {
		return false
	}
	return !isControlFree(raw)
}

// isControlFree is the Hub's printable test (no byte below 0x20 but tab and
// line breaks), which the base64 case has always used.
func isControlFree(b []byte) bool {
	for _, c := range b {
		if c < 0x20 && c != '\n' && c != '\r' && c != '\t' {
			return false
		}
	}
	return len(b) > 0
}
