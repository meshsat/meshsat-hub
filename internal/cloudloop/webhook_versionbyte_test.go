package cloudloop

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/fragment"
)

// A kit's satellite message as the Bridge puts it on the wire after egress
// transforms: the version byte 0x01, then base64 ciphertext, 405 bytes, over
// IMT. With a reassembler attached (as production has) the Hub read 0x01 as
// "fragment 1 of 2", answered fragment_buffered, and never published, stored
// or routed the message (MESHSAT-1280). Every value here is synthetic.
func TestWebhook_AVersionPrefixedMessageIsNotParkedAsAFragment(t *testing.T) {
	const imei = "300000000000001"
	body := make([]byte, 301)
	x := uint32(0x9E3779B9)
	for i := range body {
		x = x*1664525 + 1013904223
		body[i] = byte(x >> 24)
	}
	payload := append([]byte{0x01}, []byte(base64.StdEncoding.EncodeToString(body))...)
	if len(payload) != 405 || !fragment.IsFragment(payload) {
		t.Fatalf("premise gone: %d bytes, IsFragment=%v", len(payload), fragment.IsFragment(payload))
	}

	b := &ackBus{}
	h := NewWebhookHandler(b)
	h.SetReassembler(fragment.NewReassembler(time.Minute))

	mo := &LingoMO{
		ID:       "00000000-0000-4000-8000-000000000001",
		Identity: LingoIdentity{ThingID: "thing-test-1", Hardware: &LingoHardware{IMEI: imei, Type: "HARDWARE_TYPE_UNKNOWN"}},
		IMT:      &LingoIMT{CMID: "0000000000001", Topic: "IMT_TOPIC_RAW", MessageID: json.Number("1"), Size: len(payload)},
		Message:  base64.StdEncoding.EncodeToString(payload),
	}
	status := h.ProcessLingoMO(context.Background(), mo)
	if status == "fragment_buffered" || status == "fragment_error" {
		t.Fatalf("status = %q: a whole message was taken for a fragment and will never be processed", status)
	}
	raw, ok := b.msgs["meshsat/"+imei+"/mo/decoded"]
	if !ok {
		t.Fatalf("status = %q but nothing on mo/decoded; published: %v", status, b.topics())
	}
	var out WebhookMOMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Wire != mo.Message {
		t.Error("mo/decoded does not carry the payload as received; a relay would have nothing to forward")
	}
	if !out.Opaque {
		t.Error("a version-prefixed payload the Hub could not decrypt or decompress must be marked opaque")
	}
}

// Two different IMT messages from one modem are two messages. IMT has no MOMSN,
// so under the SBD id scheme both were "mo-<imei>-0": the second was taken for
// a redelivery of the first, never stored and never routed.
func TestWebhook_TwoIMTMessagesFromOneModemAreTwoMessages(t *testing.T) {
	const imei = "300000000000001"
	b := &ackBus{}
	h := NewWebhookHandler(b)

	ids := map[string]bool{}
	for _, lingoID := range []string{"00000000-0000-4000-8000-00000000000a", "00000000-0000-4000-8000-00000000000b"} {
		mo := &LingoMO{
			ID:       lingoID,
			Identity: LingoIdentity{ThingID: "thing-test-1", Hardware: &LingoHardware{IMEI: imei}},
			IMT:      &LingoIMT{CMID: "0000000000001", Topic: "IMT_TOPIC_RAW", MessageID: json.Number("1")},
			Message:  base64.StdEncoding.EncodeToString([]byte("hello " + lingoID[len(lingoID)-1:])),
		}
		h.ProcessLingoMO(context.Background(), mo)
		var out WebhookMOMessage
		if err := json.Unmarshal(b.msgs["meshsat/"+imei+"/mo/decoded"], &out); err != nil {
			t.Fatal(err)
		}
		if ids[out.ID] {
			t.Fatalf("second IMT message reused the id %q: routing and the store will treat it as a duplicate", out.ID)
		}
		ids[out.ID] = true
	}

	// SBD keeps the provider-independent scheme.
	if got := moMessageID("300000000000009", 223, "cloudloop_sbd", "some-uuid", ""); got != "mo-300000000000009-223" {
		t.Errorf("SBD id = %q, want mo-300000000000009-223 (must match rockblock.sbdMessageID)", got)
	}
}
