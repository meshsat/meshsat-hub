package bridge

import (
	"context"
	"encoding/hex"
	"testing"
)

// MESHSAT-1744: a crew's answer (0x43) and a kit's word on a chat (0x44)
// reach the crew chat service with the bearer, the origin and the payload
// after the header, and the bridge it names is reported as seen over that
// bearer.

type fakeCrewChat struct {
	calls  []string
	bridge string
}

func (f *fakeCrewChat) CrewAnswer(_ context.Context, tenantID, bearer, origin string, payload []byte) string {
	f.calls = append(f.calls, "answer "+tenantID+" "+bearer+" "+origin+" "+hex.EncodeToString(payload))
	return f.bridge
}

func (f *fakeCrewChat) CrewStatus(_ context.Context, tenantID, bearer, origin string, payload []byte) string {
	f.calls = append(f.calls, "status "+tenantID+" "+bearer+" "+origin+" "+hex.EncodeToString(payload))
	return f.bridge
}

func TestCrewChatFramesReachTheServiceWithTheirBearer(t *testing.T) {
	answer, _ := hex.DecodeString("4d530143" + "0102030405060708090a0b0c0d0e0f100a0b0c0d6ae1e3c00186206d7920f579")
	status, _ := hex.DecodeString("4d530144" + "0102030405060708090a0b0c0d0e0f100a0b0c0d6ae1e3c0040100")
	st := &fakeUplinkStore{}
	cc := &fakeCrewChat{bridge: "kit-a"}
	sink := NewUplinkSink(st, nil, nil, "test").SetCrewChat(cc)
	if !sink.Handle(context.Background(), "t1", "sms", "+31612345678", answer) {
		t.Fatal("the answer was not taken as an uplink frame")
	}
	if !sink.Handle(context.Background(), "t1", "sbd", "300234067504200", status) {
		t.Fatal("the status was not taken as an uplink frame")
	}
	want := []string{
		"answer t1 sms +31612345678 " + hex.EncodeToString(answer[4:]),
		"status t1 sbd 300234067504200 " + hex.EncodeToString(status[4:]),
	}
	if len(cc.calls) != 2 || cc.calls[0] != want[0] || cc.calls[1] != want[1] {
		t.Fatalf("calls %q, want %q", cc.calls, want)
	}
	if st.reportB != "sbd" || st.reportCall != 2 {
		t.Errorf("the kit's last report: %s after %d calls", st.reportB, st.reportCall)
	}
	// Without the service the frames are logged and dropped, still as uplink
	// frames; a refused frame reports nothing.
	plain := NewUplinkSink(st, nil, nil, "test")
	if !plain.Handle(context.Background(), "t1", "sms", "+31612345678", answer) {
		t.Error("not an uplink frame without the service")
	}
	cc.bridge = ""
	sink.Handle(context.Background(), "t1", "sms", "+31600000000", answer)
	if st.reportCall != 2 {
		t.Error("a refused frame reported the kit as seen")
	}
}
