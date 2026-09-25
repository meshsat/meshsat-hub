package cloudloop

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/fragment"
	"github.com/meshsat/meshsat-hub/internal/protocol"
)

// rnsAnnounceLike builds bytes with the shape of an RNS announce (HEADER_1,
// hops 0, ANNOUNCE, 148-byte body + 8 bytes app data). Its first byte is
// 0x01, which is also the Bridge's protocol version byte. Synthetic.
func rnsAnnounceLike() []byte {
	pkt := make([]byte, 19+148+8)
	pkt[0] = 0x01 // HEADER_1, no context flag, broadcast, SINGLE, ANNOUNCE
	pkt[1] = 0x00 // hops
	x := uint32(0x2545F491)
	for i := 2; i < len(pkt); i++ {
		x = x*1664525 + 1013904223
		pkt[i] = byte(x >> 24)
	}
	pkt[18] = 0x00 // context NONE
	return pkt
}

func rnsMO(imei, topic string, payload []byte, id string) *LingoMO {
	return &LingoMO{
		ID:       id,
		Identity: LingoIdentity{ThingID: "thing-test-9704", Hardware: &LingoHardware{IMEI: imei, Type: "ROCKBLOCK_9704"}},
		IMT:      &LingoIMT{CMID: "0000000000002", Topic: topic, MessageID: json.Number("7"), Size: len(payload)},
		Message:  base64.StdEncoding.EncodeToString(payload),
	}
}

// A Reticulum packet on the raw IMT topic, as a kit's iridium_imt_0 or
// CrossTalk's IridiumIMTInterface sends it, reaches mo/decoded opaque, with
// no text, the topic named, and the wire byte for byte, whether or not its
// first byte collides with the protocol version byte. [MESHSAT-1352]
func TestWebhook_ReticulumOnRawIMTTopicIsOpaqueAndKeepsTheWire(t *testing.T) {
	const imei = "300000000000021"
	pkt := rnsAnnounceLike()
	if !looksLikeReticulum(pkt) {
		t.Fatal("premise gone: the synthetic announce does not look like a Reticulum packet")
	}
	for name, payload := range map[string][]byte{"bare": pkt, "crosstalk_rnsi": append([]byte("RNSI\x01"), pkt...)} {
		t.Run(name, func(t *testing.T) {
			b := &ackBus{}
			h := NewWebhookHandler(b)
			h.SetReassembler(fragment.NewReassembler(time.Minute))
			mo := rnsMO(imei, IMTTopicRaw, payload, "00000000-0000-4000-8000-0000000000"+name[:2])
			status := h.ProcessLingoMO(context.Background(), mo)
			if status == "hemb_symbol" || status == "fragment_buffered" || status == "fragment_error" {
				t.Fatalf("status = %q: the packet was taken for something else and never routed", status)
			}
			raw, ok := b.msgs["meshsat/"+imei+"/mo/decoded"]
			if !ok {
				t.Fatalf("status = %q, nothing on mo/decoded; published %v", status, b.topics())
			}
			var out WebhookMOMessage
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatal(err)
			}
			if !out.Reticulum || !out.Opaque {
				t.Errorf("reticulum=%v opaque=%v, want both true", out.Reticulum, out.Opaque)
			}
			if out.Text != "" {
				t.Errorf("text = %q: a Reticulum packet must never be routed as text", out.Text)
			}
			if out.IMTTopic != IMTTopicRaw {
				t.Errorf("imt_topic = %q, want %q", out.IMTTopic, IMTTopicRaw)
			}
			if out.Wire != mo.Message {
				t.Error("wire changed: the receiving kit authenticates these bytes")
			}
		})
	}
}

// The same bytes on a colour topic are an ordinary IMT message: nothing
// claims them as Reticulum and the topic still rides along for routing.
func TestWebhook_IMTTopicIsCarriedOnEveryIMTMessage(t *testing.T) {
	const imei = "300000000000022"
	b := &ackBus{}
	h := NewWebhookHandler(b)
	mo := rnsMO(imei, IMTTopicPurple, []byte("hello purple"), "00000000-0000-4000-8000-0000000000aa")
	h.ProcessLingoMO(context.Background(), mo)
	var out WebhookMOMessage
	if err := json.Unmarshal(b.msgs["meshsat/"+imei+"/mo/decoded"], &out); err != nil {
		t.Fatal(err)
	}
	if out.IMTTopic != IMTTopicPurple || out.Reticulum || out.Text != "hello purple" {
		t.Errorf("decoded = %+v", out)
	}
}

// One in 256 arbitrary payloads used to satisfy IsHeMBFrame through the
// compact CRC-8 fallback and vanish as a "HeMB symbol" before routing. A
// Reticulum packet built to hit that coincidence must still be routed.
func TestWebhook_ACRC8CoincidenceIsNotAHeMBFrame(t *testing.T) {
	const imei = "300000000000023"
	pkt := rnsAnnounceLike()
	// Make byte 7 the CRC-8 of bytes 0-6, as the old compact check wanted.
	var crc byte
	for _, c := range pkt[:7] {
		crc ^= c
		for i := 0; i < 8; i++ {
			if crc&0x80 != 0 {
				crc = (crc << 1) ^ 0x07
			} else {
				crc <<= 1
			}
		}
	}
	pkt[7] = crc
	if protocol.IsHeMBFrame(pkt) {
		t.Fatal("IsHeMBFrame still claims a payload without the HM magic")
	}
	b := &ackBus{}
	h := NewWebhookHandler(b)
	status := h.ProcessLingoMO(context.Background(), rnsMO(imei, IMTTopicRaw, pkt, "00000000-0000-4000-8000-0000000000bb"))
	if status == "hemb_symbol" {
		t.Fatal("routed as a HeMB symbol")
	}
	if _, ok := b.msgs["meshsat/"+imei+"/mo/decoded"]; !ok {
		t.Fatalf("status = %q, nothing on mo/decoded", status)
	}
}

func TestLooksLikeReticulum(t *testing.T) {
	ann := rnsAnnounceLike()
	cases := []struct {
		name string
		raw  []byte
		want bool
	}{
		{"announce", ann, true},
		{"announce behind RNSI", append([]byte("RNSI\x01"), ann...), true},
		{"announce too short", ann[:100], false},
		{"hops 128", append([]byte{0x01, 0x80}, ann[2:]...), false},
		{"header type 3", append([]byte{0xC1, 0x00}, ann[2:]...), false},
		{"link request 64", append(make([]byte, 19), make([]byte, 64)...), false}, // packet type DATA with flags 0 -> data 64: DATA is fine
		{"text", []byte("hello from a kit, plain text over IMT"), false},
		{"text starting like a transport header", []byte("plain text over IMT that starts with a p and runs past 35 bytes"), false},
		{"version byte then base64", append([]byte{0x01}, []byte("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVowMTIzNDU2Nzg5")...), false},
		{"version byte then 404 bytes of base64 (MESHSAT-1280 shape)", append([]byte{0x01}, []byte(strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVowMTIzNDU2Nzg5", 9))...), false},
	}
	for _, c := range cases {
		got := looksLikeReticulum(c.raw)
		switch c.name {
		case "link request 64":
			// flags 0x00 = DATA; any data length is plausible, so this one IS accepted.
			if !got {
				t.Errorf("%s: DATA with 64 bytes should be accepted", c.name)
			}
		default:
			if got != c.want {
				t.Errorf("%s: %v, want %v", c.name, got, c.want)
			}
		}
	}
}
