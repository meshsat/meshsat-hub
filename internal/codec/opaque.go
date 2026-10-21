package codec

import (
	"encoding/base64"
	"unicode/utf8"

	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
)

// ReadsAsText reports whether b is UTF-8 text with no control character but
// tab and line breaks. Unlike a check for bytes below 0x20 alone, it refuses
// what random bytes look like: a 29-byte ciphertext passes the older check
// about one time in 33 ((227/256)^29).
func ReadsAsText(b []byte) bool {
	if len(b) == 0 || !utf8.Valid(b) {
		return false
	}
	for _, r := range string(b) {
		if (r < 0x20 && r != '\t' && r != '\n' && r != '\r') || r == 0x7f {
			return false
		}
	}
	return true
}

// SealedForAnother reports whether a payload the Hub could not decrypt is
// ciphertext for a key it does not hold, which the Hub relays but never reads
// as text. The Bridge puts the version byte in front only after its egress
// transforms, so versioned is the sign a chain ran; what follows is one of:
//   - base64 of binary at least a nonce and tag long: an encrypting chain
//     that ends in base64 (smaz2, encrypt, base64 on the kits until HeMB-NG);
//   - binary longer than a nonce and tag that does not read as text: an
//     encrypting chain without base64 (HeMB-NG S3-2). An MSVQ-SC frame is at
//     most 17 bytes, so it is never this long.
//
// Guessing SMAZ2 on such bytes is what made nonsense text of ciphertext.
func SealedForAnother(protoVersion byte, decrypted bool, b []byte) bool {
	if protoVersion == 0 || decrypted {
		return false
	}
	if IsBase64Ciphertext(b) {
		return true
	}
	return len(b) > hubcrypto.Overhead && !ReadsAsText(b)
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

// isControlFree is the Hub's older printable test (no byte below 0x20 but
// tab and line breaks), kept for the base64 case so that case decides as it
// always did.
func isControlFree(b []byte) bool {
	for _, c := range b {
		if c < 0x20 && c != '\n' && c != '\r' && c != '\t' {
			return false
		}
	}
	return len(b) > 0
}
