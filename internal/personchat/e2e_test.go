package personchat

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"io"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"

	"github.com/meshsat/meshsat-hub/internal/codec"
)

// sealedFrame builds what MeshSat Android sends end to end: wire v2, mode
// AES-GCM, codec byte and text inside the ciphertext.
func sealedFrame(t *testing.T, text string) string {
	t.Helper()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	ct := gcm.Seal(nil, nonce, append([]byte{codec.SMSCodecText}, text...), nil)
	frame := append(codec.SMSWireV2Header(codec.SMSModeAESGCM), append(nonce, ct...)...)
	return base64.StdEncoding.EncodeToString(frame)
}

func pgpMessage(t *testing.T, text string) (armoured string, compact string) {
	t.Helper()
	ent, err := openpgp.NewEntity("Field Person", "", "person@example.org", nil)
	if err != nil {
		t.Fatal(err)
	}
	var bin bytes.Buffer
	w, err := openpgp.Encrypt(&bin, []*openpgp.Entity{ent}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, text)
	_ = w.Close()
	var arm bytes.Buffer
	aw, _ := armor.Encode(&arm, "PGP MESSAGE", nil)
	_, _ = aw.Write(bin.Bytes())
	_ = aw.Close()
	return arm.String(), base64.StdEncoding.EncodeToString(bin.Bytes())
}

func TestSealedSMS(t *testing.T) {
	if !SealedSMS(sealedFrame(t, "on my way")) {
		t.Fatal("a sealed v2 frame must read as end to end")
	}
	clear := base64.StdEncoding.EncodeToString(append(codec.SMSWireV2Header(codec.SMSModeClear), append([]byte{codec.SMSCodecText}, "hello there my friend, the gate"...)...))
	for _, plain := range []string{
		"test", "1234", "on my way", "aGVsbG8gd29ybGQ=", clear,
		sealedFrame(t, "x") + " trailing words",
	} {
		if SealedSMS(plain) {
			t.Errorf("%q must not read as end to end", plain)
		}
	}
}

func TestPGPMessage(t *testing.T) {
	arm, compact := pgpMessage(t, "the code is 4471")
	got, ok := PGPMessage(arm)
	if !ok || got != strings.TrimSpace(arm) {
		t.Fatalf("an armoured message must be recognised and kept as it came, ok=%v", ok)
	}
	got, ok = PGPMessage(compact)
	if !ok || !strings.HasPrefix(got, "-----BEGIN PGP MESSAGE-----") {
		t.Fatalf("a compact message must be recognised and armoured, ok=%v", ok)
	}
	back, ok := CompactPGP(got)
	if !ok || back != compact {
		t.Fatal("armouring then compacting must give the device's bytes back")
	}
	if strings.Contains(got, "4471") || strings.Contains(back, "4471") {
		t.Fatal("the plaintext must not appear anywhere")
	}
	for _, plain := range []string{"", "hello", "aGVsbG8gd29ybGQ=", "-----BEGIN PGP MESSAGE-----\n\nnope\n-----END PGP MESSAGE-----"} {
		if _, ok := PGPMessage(plain); ok {
			t.Errorf("%q must not read as an OpenPGP message", plain)
		}
	}
	// A signed-only or literal message is not encrypted, so it is not end to end.
	var lit bytes.Buffer
	lw, _ := openpgp.Sign(&lit, mustEntity(t), nil, nil)
	_, _ = io.WriteString(lw, "clear words")
	_ = lw.Close()
	if _, ok := PGPMessage(base64.StdEncoding.EncodeToString(lit.Bytes())); ok {
		t.Fatal("a signed message without encryption must not read as end to end")
	}
}

func TestArmouredBlock(t *testing.T) {
	arm, _ := pgpMessage(t, "x")
	body := "Hi,\n\n" + arm + "\nregards\n> quoted"
	got, ok := ArmouredBlock(body)
	if !ok || got != strings.TrimSpace(arm) {
		t.Fatal("the armoured block inside a mail body must be found")
	}
}

func mustEntity(t *testing.T) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity("s", "", "s@example.org", nil)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
