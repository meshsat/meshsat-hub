package takchat

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/tak"
)

// The shared vector (MESHSAT-1744, docs/bridge-hub-protocol.md "Crew chat
// over satellite and SMS"): the Bridge's internal/crewchat tests hold the
// same bytes. A change here is a change there and in the doc.
const (
	vecMessageID = "01020304-0506-0708-090a-0b0c0d0e0f10"
	vecNode      = uint32(0x0a0b0c0d) // the test crew on parallax, uid meshsat-0a0b0c0d
	vecOperator  = "meshsat-hub-op-7"
	vecOpHash    = "3f24d8f92959c061"
	vecAt        = int64(1793188800) // 2026-10-28T12:00:00Z
	// C1: 'C', flags 0x05 (SMAZ2 text, reply on this bearer), message id,
	// node, operator hash, "Anna", "check in please" as SMAZ2.
	vecC1 = "4305" + "0102030405060708090a0b0c0d0e0f10" + "0a0b0c0d" + vecOpHash + "04416e6e61" + "07c48020f29794"
	// C2: the uplink header for 0x43, message id, node, unix seconds,
	// flags 0x01 (SMAZ2), "on my way".
	vecC2 = "4d530143" + "0102030405060708090a0b0c0d0e0f10" + "0a0b0c0d" + "6ae1e3c0" + "01" + "86206d7920f579"
	// C3: the uplink header for 0x44, message id, node, unix seconds,
	// status 4 (undelivered), reason 1 (not_heard), 50-byte detail.
	vecC3Detail = "the kit has not heard MESHSAT-0c0d in the last day"
	vecC3Head   = "4d530144" + "0102030405060708090a0b0c0d0e0f10" + "0a0b0c0d" + "6ae1e3c0" + "04" + "01" + "32"
)

func TestC1VectorEncodesAndDecodes(t *testing.T) {
	frame, err := EncodeChat(Chat{MessageID: vecMessageID, Node: vecNode, ReplyHere: true,
		OperatorUID: vecOperator, Callsign: "Anna", Text: "check in please"})
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(frame); got != vecC1 {
		t.Fatalf("C1\n got %s\nwant %s", got, vecC1)
	}
	if len(frame) > MaxFrameSMS {
		t.Errorf("the vector does not fit one SMS: %d bytes", len(frame))
	}
	if !IsChatFrame(frame) {
		t.Error("not recognised as a chat frame")
	}
	c, err := DecodeChat(frame)
	if err != nil {
		t.Fatal(err)
	}
	if c.MessageID != vecMessageID || c.Node != vecNode || c.ToKit || !c.ReplyHere || c.Callsign != "Anna" || c.Text != "check in please" {
		t.Errorf("decoded %+v", c)
	}
	if hex.EncodeToString(c.OperatorHash[:]) != vecOpHash || OperatorHash(vecOperator) != c.OperatorHash {
		t.Errorf("operator hash %x", c.OperatorHash)
	}
	if uid := OperatorUIDFromHash(c.OperatorHash); uid != tak.OperatorUIDPrefix+"~"+vecOpHash {
		t.Errorf("hashed uid %q", uid)
	} else if h, ok := HashFromOperatorUID(uid); !ok || h != c.OperatorHash {
		t.Errorf("hash not read back from %q", uid)
	}
	if _, ok := HashFromOperatorUID(vecOperator); ok {
		t.Error("a real operator uid read as a hash")
	}
}

func TestC1KeepsTextRawWhenSMAZ2DoesNotHelp(t *testing.T) {
	frame, err := EncodeChat(Chat{MessageID: vecMessageID, Node: 1, ToKit: true, Callsign: "Ωmega-dispatch-desk-1", Text: "日本"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := DecodeChat(frame)
	if err != nil {
		t.Fatal(err)
	}
	if c.Text != "日本" || !c.ToKit || c.ReplyHere {
		t.Errorf("decoded %+v", c)
	}
	if len(c.Callsign) > maxFrameCallsign || !strings.HasPrefix("Ωmega-dispatch-desk-1", c.Callsign) {
		t.Errorf("callsign %q", c.Callsign)
	}
	for _, bad := range [][]byte{nil, frame[:30], append([]byte{0x57}, frame[1:]...), append(frame[:31], 0x05, 0xff, 0xfe)} {
		if _, err := DecodeChat(bad); err == nil {
			t.Errorf("decoded %x", bad)
		}
	}
}

func TestC2VectorEncodesAndDecodes(t *testing.T) {
	payload, err := EncodeAnswer(Answer{MessageID: vecMessageID, Node: vecNode, At: time.Unix(vecAt, 0), Text: "on my way"})
	if err != nil {
		t.Fatal(err)
	}
	if got := "4d530143" + hex.EncodeToString(payload); got != vecC2 {
		t.Fatalf("C2\n got %s\nwant %s", got, vecC2)
	}
	a, err := DecodeAnswer(payload)
	if err != nil {
		t.Fatal(err)
	}
	if a.MessageID != vecMessageID || a.Node != vecNode || a.At.Unix() != vecAt || a.Text != "on my way" {
		t.Errorf("decoded %+v", a)
	}
	if _, err := DecodeAnswer(payload[:24]); err == nil {
		t.Error("a short answer decoded")
	}
}

func TestC3VectorEncodesAndDecodes(t *testing.T) {
	payload, err := EncodeStatus(Status{MessageID: vecMessageID, Node: vecNode, At: time.Unix(vecAt, 0),
		Status: StatusUndelivered, Reason: "not_heard", Detail: vecC3Detail})
	if err != nil {
		t.Fatal(err)
	}
	want := vecC3Head + hex.EncodeToString([]byte(vecC3Detail))
	if got := "4d530144" + hex.EncodeToString(payload); got != want {
		t.Fatalf("C3\n got %s\nwant %s", got, want)
	}
	t.Logf("C3 vector: %s", want)
	if len(payload)+4 > MaxFrameSMS {
		t.Errorf("the vector does not fit one SMS: %d bytes", len(payload)+4)
	}
	s, err := DecodeStatus(payload)
	if err != nil {
		t.Fatal(err)
	}
	if s.MessageID != vecMessageID || s.Node != vecNode || s.At.Unix() != vecAt || s.Status != StatusUndelivered ||
		s.Reason != "not_heard" || s.Detail != vecC3Detail {
		t.Errorf("decoded %+v", s)
	}
	if _, err := EncodeStatus(Status{MessageID: vecMessageID, Status: "sent"}); err == nil {
		t.Error("the Hub's own word encoded as the kit's")
	}
	bad := append([]byte(nil), payload...)
	bad[24] = 9
	if _, err := DecodeStatus(bad); err == nil {
		t.Error("an unknown status code decoded")
	}
	long, _ := EncodeStatus(Status{MessageID: vecMessageID, Status: StatusFailed, Detail: strings.Repeat("é", 60)})
	if s, err := DecodeStatus(long); err != nil || len(s.Detail) > maxFrameDetail || !strings.HasPrefix(strings.Repeat("é", 60), s.Detail) {
		t.Errorf("a long detail: %+v %v", s, err)
	}
}

func TestCrewNodeNamesAKitsCrewsOnly(t *testing.T) {
	cases := []struct {
		uid   string
		node  uint32
		toKit bool
		ok    bool
	}{
		{"meshsat-0a0b0c0d", 0x0a0b0c0d, false, true},
		{"MESHSAT-0a0b0c10", 0x0a0b0c10, true, true},
		{"MESHSAT-bridge-kit-a", 0, false, false},
		{"ANDROID-abc", 0, false, false},
		{"meshsat-zzzzzzzz", 0, false, false},
	}
	for _, c := range cases {
		node, toKit, ok := CrewNode(c.uid)
		if node != c.node || toKit != c.toKit || ok != c.ok {
			t.Errorf("%s: %x %v %v", c.uid, node, toKit, ok)
		}
		if ok && CrewUIDOf(node, toKit) != c.uid {
			t.Errorf("%s did not round-trip: %s", c.uid, CrewUIDOf(node, toKit))
		}
	}
	if ReasonCode("not_heard") != 1 || ReasonCode("send_failed") != 7 || ReasonCode("") != 0 || ReasonCode("other") != 0 {
		t.Error("reason codes")
	}
}
