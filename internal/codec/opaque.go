package codec

import (
	"context"
	"encoding/base64"

	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
)

// EgressDeclarations answers whether the bridge that owns a modem declared,
// in its last birth, that the satellite interface on that modem seals every
// payload (its send chain encrypts). Only the declaration tells ciphertext
// from plain SMAZ2: both are binary after the version byte, and a guess by
// length or printability gets one of them wrong.
type EgressDeclarations interface {
	Sealed(ctx context.Context, tenantID, deviceID string) bool
}

// SealedForAnother reports whether a payload the Hub could not decrypt is
// ciphertext for a key it does not hold, which the Hub relays but never reads
// as text. The Bridge puts the version byte in front only after its egress
// transforms, so versioned means a chain ran, not that it encrypted. It is
// sealed when:
//   - it is base64 of binary at least a nonce and tag long: an encrypting
//     chain that ends in base64 (the kits' smaz2, encrypt, base64), as the
//     Cloudloop path has always read it;
//   - or the owning bridge declared the chain sealing (declared) and it is
//     longer than a nonce and tag: an encrypting chain without base64
//     (HeMB-NG S3-2).
//
// Without a declaration, binary after the version byte is read as before: a
// plain SMAZ2 chain's output is binary too.
func SealedForAnother(protoVersion byte, decrypted bool, b []byte, declared bool) bool {
	if protoVersion == 0 || decrypted {
		return false
	}
	if IsBase64Ciphertext(b) {
		return true
	}
	return declared && len(b) > hubcrypto.Overhead
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
