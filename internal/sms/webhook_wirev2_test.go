package sms

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
)

// The app SMS wire v2 vectors (r40 review R40-R1), the Bridge's file under
// its lock: the sender's frame names the codec, so "sara" and "¡" are no
// longer the same SMS.
type wireVector struct {
	Name   string `json:"name"`
	Text   string `json:"text"`
	Mode   *int   `json:"mode"`
	Codec  *int   `json:"codec"`
	SMS    string `json:"sms"`
	Expect struct {
		Status    string `json:"status"`
		Text      string `json:"text"`
		Encrypted bool   `json:"encrypted"`
	} `json:"expect"`
}

type wireVectors struct {
	KeyHex   string       `json:"key_hex"`
	Vectors  []wireVector `json:"vectors"`
	Negative []wireVector `json:"negative"`
	Legacy   []wireVector `json:"legacy"`
}

func loadWireVectors(t *testing.T) wireVectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/sms-wire-v2-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := os.ReadFile("testdata/sms-wire-v2-vectors.lock")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if !strings.Contains(string(lock), "sha256 "+hex.EncodeToString(sum[:])+"\n") {
		t.Fatal("the vector copy is not the locked one")
	}
	var v wireVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Every vector through the real webhook: v2 is read by its codec, encrypted
// or not; v2 with a mode or codec the Hub does not read is kept as it came;
// version 1 is read by the old rule, ambiguity included.
func TestWebhookReadsAppSMSWireV2(t *testing.T) {
	v := loadWireVectors(t)
	key, _ := hex.DecodeString(v.KeyHex)
	mb := &mockBus{}
	h := NewWebhookHandler(mb, "")
	h.SetInboundAuthToken(testAuthToken)
	ks := hubcrypto.NewKeyStore()
	if _, err := ks.StoreKey("+31612345678", key, "decrypt"); err != nil {
		t.Fatal(err)
	}
	h.SetKeyStore(ks)
	for _, c := range v.Vectors {
		text, compressed, encrypted := decodedText(t, h, mb, c.SMS)
		if text != c.Expect.Text || encrypted != c.Expect.Encrypted || compressed != (*c.Codec == 1) {
			t.Errorf("%s: read %q compressed=%v encrypted=%v, want %q encrypted=%v", c.Name, text, compressed, encrypted, c.Expect.Text, c.Expect.Encrypted)
		}
	}
	for _, c := range v.Negative {
		if text, _, encrypted := decodedText(t, h, mb, c.SMS); text != c.SMS || encrypted != c.Expect.Encrypted {
			t.Errorf("%s: read %q encrypted=%v, want it kept as it came (%q) encrypted=%v", c.Name, text, encrypted, c.SMS, c.Expect.Encrypted)
		}
	}
	for _, c := range v.Legacy {
		if text, _, encrypted := decodedText(t, h, mb, c.SMS); text != c.Expect.Text || encrypted != c.Expect.Encrypted {
			t.Errorf("%s: read %q encrypted=%v, want %q encrypted=%v (the version 1 rule)", c.Name, text, encrypted, c.Expect.Text, c.Expect.Encrypted)
		}
	}
	// Without the key a sealed v2 SMS is kept, never read as text.
	nokey := NewWebhookHandler(mb, "")
	nokey.SetInboundAuthToken(testAuthToken)
	for _, c := range v.Vectors {
		if *c.Mode != 1 {
			continue
		}
		if text, _, encrypted := decodedText(t, nokey, mb, c.SMS); text != c.SMS || encrypted {
			t.Errorf("%s without the key: read %q encrypted=%v", c.Name, text, encrypted)
		}
	}
}

// The r40 review's four counterexamples, sent the v2 way: each reads as typed.
func TestWebhookReviewR40SaraSata(t *testing.T) {
	v := loadWireVectors(t)
	key, _ := hex.DecodeString(v.KeyHex)
	mb := &mockBus{}
	h := NewWebhookHandler(mb, "")
	h.SetInboundAuthToken(testAuthToken)
	ks := hubcrypto.NewKeyStore()
	if _, err := ks.StoreKey("+31612345678", key, "decrypt"); err != nil {
		t.Fatal(err)
	}
	h.SetKeyStore(ks)
	for _, name := range []string{"sara_clear", "sata_clear", "sara_aesgcm", "sata_aesgcm"} {
		var c *wireVector
		for i := range v.Vectors {
			if v.Vectors[i].Name == name {
				c = &v.Vectors[i]
			}
		}
		if c == nil {
			t.Fatalf("no vector %s", name)
		}
		raw, _ := base64.StdEncoding.DecodeString(c.SMS)
		if text, _, _ := decodedText(t, h, mb, c.SMS); text != c.Text {
			t.Errorf("%s (% x): read %q, want %q", name, raw[:3], text, c.Text)
		}
	}
}

// legacy_encrypted_clear_header_overlap (r41 review V2): an unversioned SMS
// the key opens, whose bytes also parse as a clear v2 frame with a text body
// that reads. The key's reading wins: its words, encrypted, persisted as
// decrypted. Without the key the clear frame is all there is.
func TestWebhookLegacyEncryptedClearHeaderOverlap(t *testing.T) {
	v := loadWireVectors(t)
	c := v.Legacy[len(v.Legacy)-1]
	if c.Name != "legacy_encrypted_clear_header_overlap" {
		t.Fatalf("the last legacy vector is %s", c.Name)
	}
	raw, _ := base64.StdEncoding.DecodeString(c.SMS)
	if r := readSMSWireV2(raw, func([]byte) ([]byte, bool) { return nil, false }, nil); !r.read || r.encrypted {
		t.Fatalf("the vector does not parse as a clear v2 frame: %+v", r)
	}
	key, _ := hex.DecodeString(v.KeyHex)
	mb := &mockBus{}
	h := NewWebhookHandler(mb, "")
	h.SetInboundAuthToken(testAuthToken)
	ks := hubcrypto.NewKeyStore()
	if _, err := ks.StoreKey("+31612345678", key, "decrypt"); err != nil {
		t.Fatal(err)
	}
	h.SetKeyStore(ks)
	if text, _, encrypted := decodedText(t, h, mb, c.SMS); text != c.Expect.Text || !encrypted {
		t.Fatalf("read %q encrypted=%v, want %q encrypted", text, encrypted, c.Expect.Text)
	}
}
