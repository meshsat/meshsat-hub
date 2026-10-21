package globalstar

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/codec"
	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
)

// A Bridge's ciphertext without base64 (HeMB-NG S3-2) is opaque here too.
func TestServeHTTP_CiphertextWithoutBase64IsOpaque(t *testing.T) {
	key, err := hubcrypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := hubcrypto.Encrypt(key, []byte{0x00, 'o', 'n', ' ', 'm', 'y', ' ', 'w', 'a', 'y'})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		data   []byte
		opaque bool
	}{
		{"ciphertext", append([]byte{codec.ProtoVersion1}, sealed...), true},
		{"bigram-shaped", append([]byte{codec.ProtoVersion1}, bigramShaped()...), true},
		{"text", []byte("Hello from Globalstar"), false},
	} {
		mb := &mockBus{}
		h := NewHandler(mb, testSecret)
		p := newTestPayload("dev-gs-031", "msg-"+c.name, "")
		p.Data = base64.StdEncoding.EncodeToString(c.data)
		if w := postWebhook(h, p); w.Code != 200 {
			t.Fatalf("%s: %d %s", c.name, w.Code, w.Body.String())
		}
		var out MOMessage
		for _, m := range mb.published {
			if m.topic == "meshsat/dev-gs-031/mo/decoded" {
				if err := json.Unmarshal(m.data, &out); err != nil {
					t.Fatal(err)
				}
			}
		}
		if out.Opaque != c.opaque || (c.opaque && out.Text != "") || (!c.opaque && out.Text == "") {
			t.Fatalf("%s: opaque=%v text=%q", c.name, out.Opaque, out.Text)
		}
	}
}

// bigramShaped reads as letters to the old SMAZ2 guess: bigram codes and no
// control byte, as ciphertext sometimes is.
func bigramShaped() []byte {
	b := make([]byte, 60)
	for i := range b {
		b[i] = byte(0x80 + i)
	}
	return b
}
