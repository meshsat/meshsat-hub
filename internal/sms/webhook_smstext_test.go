package sms

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/compress"
	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
)

// appSMS is a binary SMS body as MeshSat Android and iOS build it
// (SmsSender, SmsWire.encode): SMAZ2 with the default table only when it
// gains, then AES-256-GCM (12-byte nonce, ciphertext, tag) when a key is
// set, then protocol version 0x01 in front, base64.
func appSMS(t *testing.T, text string, key []byte) (body string, compressed bool) {
	t.Helper()
	payload := []byte(text)
	if z := compress.CompressDefault(payload); len(z) < len(payload) {
		payload, compressed = z, true
	}
	if key != nil {
		sealed, err := hubcrypto.Encrypt(key, payload)
		if err != nil {
			t.Fatal(err)
		}
		payload = sealed
	}
	return base64.StdEncoding.EncodeToString(append([]byte{0x01}, payload...)), compressed
}

func decodedText(t *testing.T, h *WebhookHandler, mb *mockBus, body string) (string, bool, bool) {
	t.Helper()
	mb.published = nil
	form := inboundForm()
	form.Set("Body", body)
	form.Set("MessageSid", "SM"+body[:8])
	w := httptest.NewRecorder()
	h.ServeHTTP(w, signedRequest(t, form))
	if w.Code != http.StatusOK || len(mb.published) == 0 {
		t.Fatalf("HTTP %d, %d publishes: %s", w.Code, len(mb.published), w.Body.String())
	}
	var m struct {
		Text       string `json:"text"`
		Compressed bool   `json:"compressed"`
		Encrypted  bool   `json:"encrypted"`
	}
	for _, p := range mb.published {
		if strings.HasSuffix(p.topic, "/mo/decoded") {
			if err := json.Unmarshal(p.data, &m); err != nil {
				t.Fatal(err)
			}
			return m.Text, m.Compressed, m.Encrypted
		}
	}
	t.Fatalf("no mo/decoded among %d publishes", len(mb.published))
	return "", false, false
}

// An SMS from a MeshSat app reads at the Hub as the person wrote it:
// compressed or not, encrypted or not, English or Greek. The Hub used to
// leave the version byte on, decompress with the Meshtastic table ("this is
// a test of the message" came out "node is a test of the camp") and take an
// uncompressed Greek SMS for SMAZ2. [MESHSAT-1646]
func TestWebhookReadsTheAppsSMSAsWritten(t *testing.T) {
	mb := &mockBus{}
	h := NewWebhookHandler(mb, "")
	h.SetInboundAuthToken(testAuthToken)
	key := bytes.Repeat([]byte{0x42}, 32)
	ks := hubcrypto.NewKeyStore()
	if _, err := ks.StoreKey("+31612345678", key, "decrypt"); err != nil {
		t.Fatal(err)
	}
	h.SetKeyStore(ks)
	for _, tc := range []struct {
		text      string
		encrypted bool
		wantComp  bool
	}{
		{"this is a test of the message from the station", false, true},
		{"this is the message from the station, όλοι καλά", false, true},
		{"this is a test of the message from the station", true, true},
		{"Καλημέρα από το σκάφος", true, false},
		{"café", true, false},
		{"ok 👍", true, false},
		{"Ναι", true, false},
		{"Καλημέρα", false, false},
	} {
		var k []byte
		if tc.encrypted {
			k = key
		}
		body, comp := appSMS(t, tc.text, k)
		if comp != tc.wantComp {
			t.Fatalf("%q: the sender would compress=%v, the case says %v", tc.text, comp, tc.wantComp)
		}
		text, compressed, encrypted := decodedText(t, h, mb, body)
		if text != tc.text || compressed != tc.wantComp || encrypted != tc.encrypted {
			t.Errorf("%q (encrypted %v): read %q compressed=%v encrypted=%v", tc.text, tc.encrypted, text, compressed, encrypted)
		}
	}
}

// A sender without the version byte still decrypts, even when its random
// nonce starts with 0x01 and looks like one. [MESHSAT-1646]
func TestWebhookDecryptsWithoutTheVersionByteWhenTheNonceStartsWith01(t *testing.T) {
	mb := &mockBus{}
	h := NewWebhookHandler(mb, "")
	h.SetInboundAuthToken(testAuthToken)
	key := bytes.Repeat([]byte{0x24}, 32)
	ks := hubcrypto.NewKeyStore()
	if _, err := ks.StoreKey("+31612345678", key, "decrypt"); err != nil {
		t.Fatal(err)
	}
	h.SetKeyStore(ks)
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := append([]byte{0x01}, bytes.Repeat([]byte{0x07}, 11)...)
	sealed := gcm.Seal(append([]byte{}, nonce...), nonce, []byte("Καλημέρα από το σκάφος"), nil)
	text, _, encrypted := decodedText(t, h, mb, base64.StdEncoding.EncodeToString(sealed))
	if text != "Καλημέρα από το σκάφος" || !encrypted {
		t.Fatalf("read %q encrypted=%v", text, encrypted)
	}
}

func TestSMSTextGate(t *testing.T) {
	// The round trip alone cannot tell a Greek word: all its bytes are bigram
	// codes, and the bigrams compress back to the same bytes.
	if _, ok := smaz2Text([]byte("Καλημέρα"), compress.DecompressDefault, compress.CompressDefault); !ok {
		t.Fatal("expected the round trip alone to be fooled by a Greek word")
	}
	for _, w := range []string{"Καλημέρα", "Ναι", "Όχι", "ok 👍", "café"} {
		if text, compressed, _ := smsText([]byte(w), nil); text != w || compressed {
			t.Errorf("%q read as %q compressed=%v", w, text, compressed)
		}
	}
	z := compress.CompressDefault([]byte("this is a test of the message"))
	if got, ok := smaz2Text(z, compress.DecompressDefault, compress.CompressDefault); !ok || got != "this is a test of the message" {
		t.Fatalf("default table: %q %v", got, ok)
	}
	if got, ok := smaz2Text(z, compress.Decompress, compress.Compress); ok {
		t.Fatalf("default-table bytes taken by the Meshtastic table as %q", got)
	}
}
