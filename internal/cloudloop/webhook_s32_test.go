package cloudloop

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/codec"
	"github.com/meshsat/meshsat-hub/internal/compress"
	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
	"github.com/meshsat/meshsat-hub/internal/fragment"
)

// declared is a fake of the bridges' egress declarations: the modems whose
// bridge declared a sealing satellite chain.
type declared map[string]bool

func (d declared) Sealed(_ context.Context, _ string, imei string) bool { return d[imei] }

func decodedOf(t *testing.T, b *ackBus, imei string) WebhookMOMessage {
	t.Helper()
	b.mu.Lock()
	raw, ok := b.msgs["meshsat/"+imei+"/mo/decoded"]
	b.mu.Unlock()
	if !ok {
		t.Fatalf("nothing on mo/decoded; published: %v", b.topics())
	}
	var out WebhookMOMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

const (
	sealedIMEI = "300000000000041" // its bridge declared a sealing chain
	plainIMEI  = "300000000000042" // no declaration
)

var moSeq int

func process(t *testing.T, imei, topic string, payload []byte) (WebhookMOMessage, *LingoMO) {
	t.Helper()
	b := &ackBus{}
	h := NewWebhookHandler(b)
	h.SetReassembler(fragment.NewReassembler(time.Minute))
	h.SetEgressDeclarations(declared{sealedIMEI: true})
	moSeq++
	mo := rnsMO(imei, topic, payload, fmt.Sprintf("00000000-0000-4000-8000-%012d", moSeq))
	h.ProcessLingoMO(context.Background(), mo)
	return decodedOf(t, b, imei), mo
}

// controlFree is ciphertext-shaped binary with no byte below 0x20: what the
// printable test calls text and the SMAZ2 guess turns into letters.
func controlFree(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(0x80 + (i*3)%0x80)
	}
	return b
}

// What each kind of payload becomes on mo/decoded, from a modem whose bridge
// declared a sealing chain and from one without a declaration. Plain SMAZ2
// stays readable without the declaration (R26-R1), ciphertext of a declared
// modem is opaque with no guessed text, and every message keeps its wire.
func TestWebhook_PayloadMatrix(t *testing.T) {
	short := []byte("OK at base, all safe")
	long := []byte("Team at the north gate, all safe, moving to camp")
	shortZ, longZ := compress.Compress(short), compress.Compress(long)
	if len(shortZ) > hubcrypto.Overhead || len(longZ) <= hubcrypto.Overhead {
		t.Fatalf("premise: SMAZ2 fixtures are %d and %d bytes", len(shortZ), len(longZ))
	}
	b64 := []byte(base64.StdEncoding.EncodeToString(make([]byte, 40)))
	v := func(b []byte) []byte { return append([]byte{codec.ProtoVersion1}, b...) }
	ann := rnsAnnounce(t)
	tampered := append([]byte(nil), ann...)
	tampered[len(tampered)-1] ^= 0x01

	type want struct {
		reticulum, opaque bool
		compressed        int // 1 yes, 0 no, -1 not asserted (plain ASCII reads through SMAZ2 as itself)
		text              string
	}
	skip := "\x00skip" // only the classification and the wire are asserted
	cases := []struct {
		name    string
		imei    string
		topic   string
		payload []byte
		want    want
	}{
		{"UTF-8 text", plainIMEI, IMTTopicPurple, []byte("hello from a kit, no transforms"), want{false, false, -1, "hello from a kit, no transforms"}},
		{"UTF-8 text, declared modem", sealedIMEI, IMTTopicPurple, []byte("hello from a kit, no transforms"), want{false, false, -1, "hello from a kit, no transforms"}},
		{"short SMAZ2", plainIMEI, IMTTopicRaw, v(shortZ), want{false, false, 1, string(short)}},
		{"short SMAZ2, declared modem", sealedIMEI, IMTTopicRaw, v(shortZ), want{false, false, 1, string(short)}},
		{"SMAZ2 over 28 bytes", plainIMEI, IMTTopicRaw, v(longZ), want{false, false, 1, string(long)}},
		{"SMAZ2 over 28 bytes, declared modem (read as its declaration says)", sealedIMEI, IMTTopicRaw, v(longZ), want{false, true, 0, ""}},
		{"control-free ciphertext, declared modem", sealedIMEI, IMTTopicRaw, v(controlFree(45)), want{false, true, 0, ""}},
		{"base64 ciphertext", plainIMEI, IMTTopicRaw, v(b64), want{false, true, 0, string(b64)}},
		{"base64 ciphertext, declared modem", sealedIMEI, IMTTopicRaw, v(b64), want{false, true, 0, string(b64)}},
		{"signed announce", plainIMEI, IMTTopicRaw, ann, want{true, true, 0, ""}},
		{"signed announce, declared modem", sealedIMEI, IMTTopicRaw, ann, want{true, true, 0, ""}},
		{"tampered announce, declared modem", sealedIMEI, IMTTopicRaw, tampered, want{false, true, 0, ""}},
		{"tampered announce", plainIMEI, IMTTopicRaw, tampered, want{false, false, -1, skip}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, mo := process(t, c.imei, c.topic, c.payload)
			if out.Wire != mo.Message {
				t.Fatal("the wire is not kept byte for byte")
			}
			if out.Reticulum != c.want.reticulum {
				t.Fatalf("reticulum=%v", out.Reticulum)
			}
			if c.want.text == skip {
				return
			}
			if c.want.compressed >= 0 && out.Compressed != (c.want.compressed == 1) {
				t.Fatalf("compressed=%v", out.Compressed)
			}
			if out.Opaque != c.want.opaque || out.Text != c.want.text {
				t.Fatalf("opaque=%v compressed=%v text=%q", out.Opaque, out.Compressed, out.Text)
			}
		})
	}
}

// A declared modem's real AES-GCM output, every length from the shortest
// frame up and many nonces: always opaque, never text, never Reticulum, the
// wire kept for the relay.
func TestWebhook_DeclaredCiphertextIsOpaque(t *testing.T) {
	rng := rand.New(rand.NewSource(1584))
	key, err := hubcrypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	for n := 2; n <= 400; n++ {
		for k := 0; k < 3; k++ {
			frame := make([]byte, n) // a smaz2g frame: mode byte, then data
			rng.Read(frame)
			frame[0] = byte(k % 2)
			sealed, err := hubcrypto.Encrypt(key, frame)
			if err != nil {
				t.Fatal(err)
			}
			out, mo := process(t, sealedIMEI, IMTTopicRaw, append([]byte{codec.ProtoVersion1}, sealed...))
			if out.Reticulum || !out.Opaque || out.Text != "" || out.Compressed || out.Wire != mo.Message {
				t.Fatalf("%d bytes: reticulum=%v opaque=%v text=%q compressed=%v", len(sealed)+1, out.Reticulum, out.Opaque, out.Text, out.Compressed)
			}
		}
	}
}

// What the Hub cannot turn into text goes only to destinations that carry an
// opaque payload: before, it was published with empty text and not opaque.
func TestWebhook_UnreadablePayloadIsOpaque(t *testing.T) {
	out, _ := process(t, plainIMEI, IMTTopicPurple, []byte{0x02, 0x00, 0xff, 0x10, 0x80, 0x00, 0x03})
	if out.Text != "" || !out.Opaque {
		t.Fatalf("text=%q opaque=%v", out.Text, out.Opaque)
	}
}
