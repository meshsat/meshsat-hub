package cloudloop

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bridge"
	"github.com/meshsat/meshsat-hub/internal/codec"
	"github.com/meshsat/meshsat-hub/internal/compress"
	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
	"github.com/meshsat/meshsat-hub/internal/fragment"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// declared is a fake of the bridges' egress declarations: the modems whose
// bridge declared a sealing satellite chain.
type declared map[string]codec.Declaration

func (d declared) Declaration(_ context.Context, _ string, imei string) codec.Declaration {
	return d[imei]
}

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
	h.SetEgressDeclarations(declared{sealedIMEI: codec.DeclaredSealed})
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

// egressDouble is a declaration store for the real bridge.EgressDeclarations:
// one owner ("kit"), its durable record, its last birth, and a lookup error.
type egressDouble struct {
	record    [2]string
	mapFormat string // what the bridge's merged declarations say
	birth     string
	err       error
}

func (s *egressDouble) BridgeEgressEntry(context.Context, string, string, string) (store.EgressDecl, error) {
	return store.EgressDecl{Format: s.mapFormat}, s.err
}

// heldDeclaration is a subscriber's pending declaration (SetPending).
type heldDeclaration struct{ bridge, format string }

func (h heldDeclaration) PendingEgressEntry(string, string) (string, store.EgressDecl, bool) {
	return h.bridge, store.EgressDecl{Format: h.format}, true
}

func (s *egressDouble) DeviceBridgeID(context.Context, string, string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return "kit", nil
}

func (s *egressDouble) GetBridge(context.Context, string, string) (*store.Bridge, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &store.Bridge{BridgeID: "kit", LastBirth: s.birth}, nil
}

func (s *egressDouble) DeviceEgress(context.Context, string, string) (string, string, error) {
	return s.record[0], s.record[1], s.err
}

// fixedCiphertext is the r27 review's fixture: AES-256-GCM under an all-zero
// test key of the smaz2g frame {0x00, 'k'}, which the SMAZ2 guess reads as
// text; with the version byte, 31 bytes.
func fixedCiphertext(t *testing.T) []byte {
	t.Helper()
	b, err := hex.DecodeString("42f28a46039f894d3a0179d07a753f51ccddb2686d236a400d8ee239f8d1")
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte{codec.ProtoVersion1}, b...)
}

// The declaration's life at the Cloudloop boundary, through the real reader:
// a known sealing modem stays opaque when its record and birth lose the
// declaration or the lookup fails; plain, declared or not, reads as text,
// including after an explicit change from sealed.
func TestWebhook_DeclarationLifecycle(t *testing.T) {
	long := "Team at the north gate, all safe, moving to camp"
	smaz := append([]byte{codec.ProtoVersion1}, compress.Compress([]byte(long))...)
	sealedBirth := `{"bridge_id":"kit","interfaces":[{"name":"iridium_imt","imei":"` + sealedIMEI + `","egress_format":"sealed","egress_sealed":true}]}`
	noIMEIBirth := `{"bridge_id":"kit","interfaces":[{"name":"iridium_imt","imei":"","egress_format":"sealed","egress_sealed":true}]}`
	run := func(t *testing.T, d *bridge.EgressDeclarations, payload []byte) WebhookMOMessage {
		t.Helper()
		b := &ackBus{}
		h := NewWebhookHandler(b)
		h.SetEgressDeclarations(d)
		moSeq++
		mo := rnsMO(sealedIMEI, IMTTopicRaw, payload, fmt.Sprintf("00000000-0000-4000-8000-%012d", moSeq))
		h.ProcessLingoMO(context.Background(), mo)
		out := decodedOf(t, b, sealedIMEI)
		if out.Wire != mo.Message {
			t.Fatal("the wire is not kept")
		}
		return out
	}
	opaque := func(t *testing.T, out WebhookMOMessage) {
		t.Helper()
		if !out.Opaque || out.Text != "" || out.Compressed {
			t.Fatalf("opaque=%v text=%q compressed=%v", out.Opaque, out.Text, out.Compressed)
		}
	}
	readable := func(t *testing.T, out WebhookMOMessage) {
		t.Helper()
		if out.Opaque || out.Text != long || !out.Compressed {
			t.Fatalf("opaque=%v text=%q compressed=%v", out.Opaque, out.Text, out.Compressed)
		}
	}
	t.Run("declared sealed", func(t *testing.T) {
		opaque(t, run(t, bridge.NewEgressDeclarations(&egressDouble{record: [2]string{"kit", "sealed"}}), fixedCiphertext(t)))
	})
	t.Run("record and birth lose it after it was confirmed", func(t *testing.T) {
		s := &egressDouble{birth: sealedBirth}
		d := bridge.NewEgressDeclarations(s)
		opaque(t, run(t, d, fixedCiphertext(t)))
		s.birth = noIMEIBirth
		opaque(t, run(t, d, fixedCiphertext(t)))
	})
	t.Run("lookup fails, nothing confirmed", func(t *testing.T) {
		opaque(t, run(t, bridge.NewEgressDeclarations(&egressDouble{err: errors.New("db down")}), fixedCiphertext(t)))
	})
	t.Run("declared plain", func(t *testing.T) {
		readable(t, run(t, bridge.NewEgressDeclarations(&egressDouble{record: [2]string{"kit", "plain"}}), smaz))
	})
	t.Run("sealed, then an explicit change to plain", func(t *testing.T) {
		s := &egressDouble{record: [2]string{"kit", "sealed"}}
		d := bridge.NewEgressDeclarations(s)
		opaque(t, run(t, d, smaz))
		s.record = [2]string{"kit", "plain"}
		readable(t, run(t, d, smaz))
	})
	t.Run("undeclared", func(t *testing.T) {
		readable(t, run(t, bridge.NewEgressDeclarations(&egressDouble{birth: noIMEIBirth}), smaz))
	})
	// R28-R1: the record says plain because its write failed.
	t.Run("the bridge's declarations say sealed over an older plain record", func(t *testing.T) {
		opaque(t, run(t, bridge.NewEgressDeclarations(&egressDouble{record: [2]string{"kit", "plain"}, mapFormat: "sealed"}), fixedCiphertext(t)))
	})
	t.Run("a held sealed over an older plain record and map", func(t *testing.T) {
		d := bridge.NewEgressDeclarations(&egressDouble{record: [2]string{"kit", "plain"}, mapFormat: "plain"})
		d.SetPending(heldDeclaration{"kit", "sealed"})
		opaque(t, run(t, d, fixedCiphertext(t)))
	})
	t.Run("a held plain over an older sealed", func(t *testing.T) {
		d := bridge.NewEgressDeclarations(&egressDouble{record: [2]string{"kit", "sealed"}, mapFormat: "sealed"})
		d.SetPending(heldDeclaration{"kit", "plain"})
		readable(t, run(t, d, smaz))
	})
	// R30-R1: a plain this reader confirmed, or holds, does not answer a
	// failed lookup: a newer sealed may have been stored meanwhile.
	t.Run("plain confirmed, then the lookup fails", func(t *testing.T) {
		s := &egressDouble{record: [2]string{"kit", "plain"}}
		d := bridge.NewEgressDeclarations(s)
		readable(t, run(t, d, smaz))
		s.err = errors.New("db down")
		opaque(t, run(t, d, fixedCiphertext(t)))
		d.SetPending(heldDeclaration{"kit", "plain"})
		opaque(t, run(t, d, fixedCiphertext(t)))
		s.err = nil
		readable(t, run(t, d, smaz))
	})
}
