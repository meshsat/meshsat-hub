package globalstar

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/codec"
	"github.com/meshsat/meshsat-hub/internal/compress"
	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
)

type declared map[string]bool

func (d declared) Sealed(_ context.Context, _ string, id string) bool { return d[id] }

// The Globalstar webhook decides as the Cloudloop and Rock7 ones.
func TestServeHTTP_PayloadMatrix(t *testing.T) {
	short := "OK at base, all safe"
	long := "Team at the north gate, all safe, moving to camp"
	v := func(b []byte) []byte { return append([]byte{codec.ProtoVersion1}, b...) }
	ctrlFree := make([]byte, 45)
	for i := range ctrlFree {
		ctrlFree[i] = byte(0x80 + (i*3)%0x80)
	}
	b64 := []byte(base64.StdEncoding.EncodeToString(make([]byte, 40)))
	key, err := hubcrypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := hubcrypto.Encrypt(key, []byte{0x00, 'o', 'n', ' ', 'm', 'y', ' ', 'w', 'a', 'y'})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name       string
		device     string
		data       []byte
		opaque     bool
		compressed int // 1 yes, 0 no, -1 not asserted
		text       string
	}{
		{"UTF-8 text", "dev-plain", []byte("Hello from Globalstar"), false, -1, "Hello from Globalstar"},
		{"short SMAZ2", "dev-plain", v(compress.Compress([]byte(short))), false, 1, short},
		{"SMAZ2 over 28 bytes", "dev-plain", v(compress.Compress([]byte(long))), false, 1, long},
		{"SMAZ2 over 28 bytes, declared device", "dev-sealed", v(compress.Compress([]byte(long))), true, 0, ""},
		{"ciphertext, declared device", "dev-sealed", v(sealed), true, 0, ""},
		{"control-free ciphertext, declared device", "dev-sealed", v(ctrlFree), true, 0, ""},
		{"base64 ciphertext", "dev-plain", v(b64), true, 0, string(b64)},
	} {
		mb := &mockBus{}
		h := NewHandler(mb, testSecret)
		h.SetEgressDeclarations(declared{"dev-sealed": true})
		p := newTestPayload(c.device, "msg-"+c.name, "")
		p.Data = base64.StdEncoding.EncodeToString(c.data)
		if w := postWebhook(h, p); w.Code != 200 {
			t.Fatalf("%s: %d %s", c.name, w.Code, w.Body.String())
		}
		var out MOMessage
		for _, m := range mb.published {
			if m.topic == "meshsat/"+c.device+"/mo/decoded" {
				if err := json.Unmarshal(m.data, &out); err != nil {
					t.Fatal(err)
				}
			}
		}
		if out.Raw != base64.StdEncoding.EncodeToString(c.data) {
			t.Errorf("%s: the payload is not kept as received", c.name)
		}
		if c.compressed >= 0 && out.Compressed != (c.compressed == 1) {
			t.Errorf("%s: compressed=%v", c.name, out.Compressed)
		}
		if out.Opaque != c.opaque || out.Text != c.text {
			t.Errorf("%s: opaque=%v text=%q", c.name, out.Opaque, out.Text)
		}
	}
}
