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
		Status string `json:"status"`
		Text   string `json:"text"`
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
		if text != c.Expect.Text || encrypted != (*c.Mode == 1) || compressed != (*c.Codec == 1) {
			t.Errorf("%s: read %q compressed=%v encrypted=%v, want %q", c.Name, text, compressed, encrypted, c.Expect.Text)
		}
	}
	for _, c := range v.Negative {
		if text, _, _ := decodedText(t, h, mb, c.SMS); text != c.SMS {
			t.Errorf("%s: read %q, want it kept as it came (%q)", c.Name, text, c.SMS)
		}
	}
	for _, c := range v.Legacy {
		if text, _, _ := decodedText(t, h, mb, c.SMS); text != c.Expect.Text {
			t.Errorf("%s: read %q, want %q (the version 1 rule)", c.Name, text, c.Expect.Text)
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
