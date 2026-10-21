package codec

import (
	"encoding/base64"
	"math/rand"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/compress"
	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
)

// controlFreeCiphertext is a fixed 45-byte AES-GCM-shaped payload with no
// byte below 0x20: what the printable test calls text and SMAZ2 makes
// letters of. Fixed, so the case does not depend on a random draw.
var controlFreeCiphertext = func() []byte {
	b := make([]byte, 45)
	for i := range b {
		b[i] = byte(0x80 + (i*3)%0x80)
	}
	return b
}()

func TestSealedForAnother(t *testing.T) {
	plain := compress.Compress([]byte("Team at the north gate, all safe, moving to camp"))
	if len(plain) <= hubcrypto.Overhead {
		t.Fatalf("premise: the SMAZ2 fixture is %d bytes, it must be longer than a nonce and tag", len(plain))
	}
	b64 := []byte(base64.StdEncoding.EncodeToString(make([]byte, 40)))
	for _, c := range []struct {
		name      string
		version   byte
		decrypted bool
		b         []byte
		declared  Declaration
		want      bool
	}{
		{"base64 ciphertext, no declaration", ProtoVersion1, false, b64, Undeclared, true},
		{"base64 ciphertext, declared", ProtoVersion1, false, b64, DeclaredSealed, true},
		{"plain SMAZ2 over 28 bytes, no declaration", ProtoVersion1, false, plain, Undeclared, false},
		{"plain SMAZ2 over 28 bytes, declared plain", ProtoVersion1, false, plain, DeclaredPlain, false},
		{"control-free ciphertext, no declaration", ProtoVersion1, false, controlFreeCiphertext, Undeclared, false},
		{"control-free ciphertext, declared plain", ProtoVersion1, false, controlFreeCiphertext, DeclaredPlain, false},
		{"control-free ciphertext, declared", ProtoVersion1, false, controlFreeCiphertext, DeclaredSealed, true},
		{"control-free ciphertext, declaration unavailable", ProtoVersion1, false, controlFreeCiphertext, DeclarationUnavailable, true},
		{"declared, but too short for a nonce and tag", ProtoVersion1, false, controlFreeCiphertext[:hubcrypto.Overhead], DeclaredSealed, false},
		{"declared, MSVQ-SC sized", ProtoVersion1, false, []byte{0x31, 0, 1, 0, 2, 0, 3}, DeclaredSealed, false},
		{"unavailable, MSVQ-SC sized", ProtoVersion1, false, []byte{0x31, 0, 1, 0, 2, 0, 3}, DeclarationUnavailable, false},
		{"no version byte", 0, false, controlFreeCiphertext, DeclaredSealed, false},
		{"decrypted by the Hub", ProtoVersion1, true, controlFreeCiphertext, DeclaredSealed, false},
	} {
		if got := SealedForAnother(c.version, c.decrypted, c.b, c.declared); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// Invariants over real AES-GCM output: a declared or unavailable modem's
// ciphertext is always sealed, an undeclared or declared-plain modem's
// binary never is (it is read as before). No random draw has to hit anything.
func TestSealedForAnotherFollowsTheDeclaration(t *testing.T) {
	rng := rand.New(rand.NewSource(1584))
	key, err := hubcrypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 400; n += 3 {
		plain := make([]byte, n)
		rng.Read(plain)
		sealed, err := hubcrypto.Encrypt(key, plain)
		if err != nil {
			t.Fatal(err)
		}
		if !SealedForAnother(ProtoVersion1, false, sealed, DeclaredSealed) || !SealedForAnother(ProtoVersion1, false, sealed, DeclarationUnavailable) {
			t.Fatalf("%d-byte ciphertext of a declared or unavailable modem is not sealed", len(sealed))
		}
		if SealedForAnother(ProtoVersion1, false, sealed, Undeclared) || SealedForAnother(ProtoVersion1, false, sealed, DeclaredPlain) {
			t.Fatalf("%d-byte binary of an undeclared or plain modem is sealed", len(sealed))
		}
	}
}
