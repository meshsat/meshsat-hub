package cloudloop

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/codec"
	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
	"github.com/meshsat/meshsat-hub/internal/fragment"
)

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

// A kit's satellite message once its chain drops base64 (HeMB-NG S3-2): the
// version byte, then AES-GCM output for a key the Hub does not hold. On the
// raw IMT topic it is neither a Reticulum packet nor text: it reaches
// mo/decoded opaque, with no text, and the wire byte for byte for the relay.
// Every length from the shortest frame up, many nonces each: before S3-2
// some came out as Reticulum (shape) or as SMAZ2 nonsense (printable test).
func TestWebhook_KitCiphertextWithoutBase64IsOpaque(t *testing.T) {
	const imei = "300000000000031"
	rng := rand.New(rand.NewSource(1584))
	key, err := hubcrypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	b := &ackBus{}
	h := NewWebhookHandler(b)
	h.SetReassembler(fragment.NewReassembler(time.Minute))
	i := 0
	for n := 2; n <= 400; n++ {
		for k := 0; k < 3; k++ {
			frame := make([]byte, n) // a smaz2g frame: mode byte, then data
			rng.Read(frame)
			frame[0] = byte(k % 2)
			sealed, err := hubcrypto.Encrypt(key, frame)
			if err != nil {
				t.Fatal(err)
			}
			payload := append([]byte{codec.ProtoVersion1}, sealed...)
			i++
			mo := rnsMO(imei, IMTTopicRaw, payload, fmt.Sprintf("00000000-0000-4000-8000-%012d", i))
			status := h.ProcessLingoMO(context.Background(), mo)
			if status == "hemb_symbol" || status == "fragment_buffered" || status == "fragment_error" || status == "oob_frame" {
				t.Fatalf("%d bytes: status %q", len(payload), status)
			}
			out := decodedOf(t, b, imei)
			if out.Reticulum {
				t.Fatalf("%d bytes: taken for a Reticulum packet", len(payload))
			}
			if !out.Opaque || out.Text != "" || out.Compressed {
				t.Fatalf("%d bytes: opaque=%v text=%q compressed=%v", len(payload), out.Opaque, out.Text, out.Compressed)
			}
			if out.Wire != mo.Message {
				t.Fatalf("%d bytes: the relay would not carry the bytes as received", len(payload))
			}
		}
	}
}

// Ciphertext that reads as letters to the SMAZ2 guess (bigram codes, no
// control byte) is still ciphertext.
func TestWebhook_BigramShapedCiphertextIsOpaque(t *testing.T) {
	const imei = "300000000000034"
	payload := []byte{codec.ProtoVersion1}
	for i := 0; i < 60; i++ {
		payload = append(payload, byte(0x80+i))
	}
	b := &ackBus{}
	h := NewWebhookHandler(b)
	h.ProcessLingoMO(context.Background(), rnsMO(imei, IMTTopicRaw, payload, "00000000-0000-4000-8000-0000000000e1"))
	out := decodedOf(t, b, imei)
	if !out.Opaque || out.Text != "" || out.Compressed {
		t.Fatalf("opaque=%v text=%q compressed=%v", out.Opaque, out.Text, out.Compressed)
	}
}

// What the Hub cannot turn into text goes only to destinations that carry an
// opaque payload: before, it was published with empty text and not opaque,
// so a route to a person fired with nothing in it.
func TestWebhook_UnreadablePayloadIsOpaque(t *testing.T) {
	const imei = "300000000000032"
	b := &ackBus{}
	h := NewWebhookHandler(b)
	payload := []byte{0x02, 0x00, 0xff, 0x10, 0x80, 0x00, 0x03}
	mo := rnsMO(imei, IMTTopicPurple, payload, "00000000-0000-4000-8000-0000000000c1")
	h.ProcessLingoMO(context.Background(), mo)
	out := decodedOf(t, b, imei)
	if out.Text != "" || !out.Opaque {
		t.Fatalf("text=%q opaque=%v", out.Text, out.Opaque)
	}
}

// Text, versioned or not, is still read as text.
func TestWebhook_TextIsNotOpaque(t *testing.T) {
	const imei = "300000000000033"
	for i, payload := range [][]byte{
		[]byte("hello from a kit over IMT, no transforms"),
		append([]byte{codec.ProtoVersion1}, []byte("a chain with no encryption step at all")...),
	} {
		b := &ackBus{}
		h := NewWebhookHandler(b)
		h.ProcessLingoMO(context.Background(), rnsMO(imei, IMTTopicPurple, payload, fmt.Sprintf("00000000-0000-4000-8000-0000000000d%d", i)))
		out := decodedOf(t, b, imei)
		if out.Opaque || out.Text == "" {
			t.Fatalf("%q: text=%q opaque=%v", payload, out.Text, out.Opaque)
		}
	}
}
