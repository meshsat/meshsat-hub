package codec

import (
	"encoding/base64"
	"math/rand"
	"testing"

	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
)

func TestReadsAsText(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"hello from a kit", true},
		{"Καλημέρα\tαπό\r\nτη βάση", true},
		{"", false},
		{"nul\x00inside", false},
		{"\x01smaz2 code byte", false},
		{"del\x7f", false},
		{"\xff\xfe not UTF-8", false},
	} {
		if got := ReadsAsText([]byte(c.in)); got != c.want {
			t.Errorf("%q: %v, want %v", c.in, got, c.want)
		}
	}
}

// A kit's chain without base64 (HeMB-NG S3-2) sends the version byte and
// AES-GCM output. Every such payload the Hub cannot open is ciphertext for
// another key, whatever its length; the older printable test let about one
// 29-byte ciphertext in fifty through to the SMAZ2 guess.
func TestSealedForAnotherBinaryCiphertext(t *testing.T) {
	rng := rand.New(rand.NewSource(1584))
	key, err := hubcrypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	controlFree := 0
	for n := 1; n <= 400; n++ {
		for k := 0; k < 20; k++ {
			plain := make([]byte, n)
			rng.Read(plain)
			sealed, err := hubcrypto.Encrypt(key, plain)
			if err != nil {
				t.Fatal(err)
			}
			if isControlFree(sealed) {
				controlFree++
			}
			if !SealedForAnother(ProtoVersion1, false, sealed) {
				t.Fatalf("%d-byte ciphertext read as something the Hub may guess at", len(sealed))
			}
			if SealedForAnother(0, false, sealed) || SealedForAnother(ProtoVersion1, true, sealed) {
				t.Fatal("without the version byte, or once decrypted, a payload is not sealed for another")
			}
		}
	}
	if controlFree == 0 {
		t.Fatal("premise gone: no ciphertext passed the older printable test, so this test shows nothing")
	}
	t.Logf("%d of 8000 ciphertexts had no control byte (the older printable test called them text)", controlFree)
}

func TestSealedForAnotherLeavesTextAndShortFrames(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []byte
		want bool
	}{
		{"versioned text", []byte("a plain text sent through a chain without encryption"), false},
		{"versioned greek", []byte("Η ομάδα είναι ασφαλής στη βόρεια πύλη"), false},
		{"short binary, no room for a nonce and tag", []byte{0x84, 0xb1, 0x6f, 0x08, 0x8f, 0x2c}, false},
		{"MSVQ-SC frame", []byte{0x31, 0, 1, 0, 2, 0, 3}, false},
		{"base64 ciphertext", []byte(base64.StdEncoding.EncodeToString(make([]byte, 40))), true},
	} {
		if got := SealedForAnother(ProtoVersion1, false, c.in); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
