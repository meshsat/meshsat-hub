package personchat

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/meshsat/meshsat-hub/internal/codec"
)

// End to end, or plain: the body says which, by its structure, and the Hub
// never guesses past that. A body that is one of the two shapes below is
// carried exactly as the sender made it, in both directions: never prefixed,
// cut, re-encrypted or logged. Anything else is plain text the Hub may label
// and fit.
//
//	SMS:   one base64 token of a sealed MeshSat SMS frame, wire v2 mode
//	       AES-GCM (codec.SMSWireV2): 02 01 | nonce(12) | ciphertext | tag(16),
//	       sealed with the key the device and the person's MeshSat Android
//	       share. Version 1 frames say nothing about themselves, so they are
//	       not recognised: a sender that wants end to end writes v2.
//	email: an OpenPGP message, ASCII-armoured, or the same packets as one
//	       base64 token, the form a satellite link can afford. The first
//	       packet is a key packet (public-key or passphrase), an encrypted
//	       data packet follows.
//
// A plain text cannot take either shape by accident: a v2 frame starts with
// two control bytes, and a PGP stream with a packet tag and a length that has
// to parse.

// minSealedSMS is the smallest sealed v2 frame: header, nonce, a codec byte,
// tag.
const minSealedSMS = 2 + 12 + 1 + 16

// maxPGPBytes bounds what the Hub parses as an OpenPGP message.
const maxPGPBytes = 64 << 10

// SealedSMS reports whether body is an end-to-end SMS frame.
func SealedSMS(body string) bool {
	tok := strings.TrimSpace(body)
	if tok == "" || strings.ContainsAny(tok, " \t\r\n") {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(tok)
	if err != nil {
		return false
	}
	mode, payload, ok := codec.ParseSMSWireV2(raw)
	return ok && mode == codec.SMSModeAESGCM && len(payload)+2 >= minSealedSMS
}

// PGPMessage reports whether body is an OpenPGP message, and returns it
// ASCII-armoured, the form a mail client opens. A compact body (base64 of the
// binary packets) is armoured here; an armoured one is returned as it came,
// so a signature over the armour is not disturbed.
func PGPMessage(body string) (string, bool) {
	t := strings.TrimSpace(body)
	if t == "" || len(t) > maxPGPBytes*2 {
		return "", false
	}
	if strings.HasPrefix(t, "-----BEGIN PGP MESSAGE-----") {
		bin, ok := dearmour(t)
		if !ok || !isPGPMessage(bin) {
			return "", false
		}
		return t, true
	}
	if strings.ContainsAny(t, " \t\r\n") {
		return "", false
	}
	bin, err := base64.StdEncoding.DecodeString(t)
	if err != nil || !isPGPMessage(bin) {
		return "", false
	}
	return armour(bin)
}

// CompactPGP is the device's form of an armoured OpenPGP message: the
// packets as one base64 token, without the armour's header lines, checksum
// and line breaks, which a satellite MT pays for byte by byte.
func CompactPGP(armoured string) (string, bool) {
	bin, ok := dearmour(strings.TrimSpace(armoured))
	if !ok || !isPGPMessage(bin) {
		return "", false
	}
	return base64.StdEncoding.EncodeToString(bin), true
}

// ArmouredBlock returns the first armoured OpenPGP message inside text (a
// mail body may carry one with a greeting around it), or false.
func ArmouredBlock(text string) (string, bool) {
	const begin, end = "-----BEGIN PGP MESSAGE-----", "-----END PGP MESSAGE-----"
	i := strings.Index(text, begin)
	if i < 0 {
		return "", false
	}
	j := strings.Index(text[i:], end)
	if j < 0 {
		return "", false
	}
	block := text[i : i+j+len(end)]
	if _, ok := dearmour(block); !ok {
		return "", false
	}
	return block, true
}

func dearmour(t string) ([]byte, bool) {
	blk, err := armor.Decode(strings.NewReader(t))
	if err != nil || blk.Type != "PGP MESSAGE" {
		return nil, false
	}
	bin, err := io.ReadAll(io.LimitReader(blk.Body, maxPGPBytes+1))
	if err != nil || len(bin) == 0 || len(bin) > maxPGPBytes {
		return nil, false
	}
	return bin, true
}

func armour(bin []byte) (string, bool) {
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, "PGP MESSAGE", nil)
	if err != nil {
		return "", false
	}
	if _, err := w.Write(bin); err != nil {
		return "", false
	}
	if err := w.Close(); err != nil {
		return "", false
	}
	return buf.String(), true
}

// isPGPMessage: one or more key packets, then an encrypted data packet. The
// packets are parsed, not decrypted; nothing here holds a key.
func isPGPMessage(bin []byte) bool {
	if len(bin) == 0 || len(bin) > maxPGPBytes {
		return false
	}
	r := packet.NewReader(bytes.NewReader(bin))
	keys := 0
	for i := 0; i < 64; i++ {
		p, err := r.Next()
		if err != nil {
			var unsupported pgperrors.UnsupportedError
			if errors.As(err, &unsupported) {
				continue
			}
			return false
		}
		switch p.(type) {
		case *packet.EncryptedKey, *packet.SymmetricKeyEncrypted:
			keys++
		case *packet.SymmetricallyEncrypted, *packet.AEADEncrypted:
			return keys > 0
		default:
			return false
		}
	}
	return false
}
