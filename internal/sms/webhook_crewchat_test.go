package sms

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// MESHSAT-1744: a kit's crew chat frame, base64 in one SMS from its own
// number, reaches the crew chat service through the uplink sink with
// bearer "sms" and the sender as origin, and never enters the message
// pipeline.

type fakeCrewChat struct{ calls []string }

func (f *fakeCrewChat) CrewAnswer(_ context.Context, tenantID, bearer, origin string, payload []byte) string {
	f.calls = append(f.calls, "answer "+tenantID+" "+bearer+" "+origin+" "+hex.EncodeToString(payload))
	return "kit-a"
}

func (f *fakeCrewChat) CrewStatus(_ context.Context, tenantID, bearer, origin string, payload []byte) string {
	f.calls = append(f.calls, "status "+tenantID+" "+bearer+" "+origin+" "+hex.EncodeToString(payload))
	return "kit-a"
}

func TestWebhook_CrewChatFrameReachesTheService(t *testing.T) {
	bus := &mockBus{}
	h := NewWebhookHandler(bus, "")
	h.SetInboundAuthToken(testAuthToken)
	cc := &fakeCrewChat{}
	h.SetCrewChat(cc)
	status, _ := hex.DecodeString("4d530144" + "0102030405060708090a0b0c0d0e0f10a1b3c2ec6ae1e3c0040100")
	form := url.Values{"From": {"+31653618463"}, "To": {"+3197010258258"}, "Body": {base64.StdEncoding.EncodeToString(status)}, "MessageSid": {"SM-c3"}}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, signedRequest(t, form))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "bridge_uplink") {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	want := "status default sms +31653618463 " + hex.EncodeToString(status[4:])
	if len(cc.calls) != 1 || cc.calls[0] != want {
		t.Fatalf("calls %q, want %q", cc.calls, want)
	}
	if len(bus.published) != 0 {
		t.Errorf("a frame entered the pipeline: %v", bus.published)
	}
}
