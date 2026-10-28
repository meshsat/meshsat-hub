package rockblock

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// MESHSAT-1744: a kit's crew chat frame as an SBD MO reaches the crew chat
// service through the uplink sink with bearer "sbd" and the modem's IMEI as
// origin, and never enters the message pipeline.

type fakeCrewChat struct{ calls []string }

func (f *fakeCrewChat) CrewAnswer(_ context.Context, tenantID, bearer, origin string, payload []byte) string {
	f.calls = append(f.calls, "answer "+tenantID+" "+bearer+" "+origin+" "+hex.EncodeToString(payload))
	return "kit-a"
}

func (f *fakeCrewChat) CrewStatus(_ context.Context, tenantID, bearer, origin string, payload []byte) string {
	f.calls = append(f.calls, "status "+tenantID+" "+bearer+" "+origin+" "+hex.EncodeToString(payload))
	return "kit-a"
}

func TestHandler_CrewChatFrameReachesTheService(t *testing.T) {
	const imei = "300234067504200"
	useTestKey(t)
	b := &ackBus{}
	h := NewHandler(b, testSecret)
	cc := &fakeCrewChat{}
	h.SetCrewChat(cc)
	answer, _ := hex.DecodeString("4d530143" + "0102030405060708090a0b0c0d0e0f10a1b3c2ec6ae1e3c00186206d7920f579")
	dataHex := hex.EncodeToString(answer)
	form := url.Values{"imei": {imei}, "momsn": {jsonInt(41)}, "transmit_time": {"26-10-28 12:00:00"}, "data": {dataHex}}
	form.Set("JWT", signedJWT(t, jwt.MapClaims{"imei": imei, "momsn": 41, "data": dataHex, "transmit_time": form.Get("transmit_time")}))
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/rockblock", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "bridge_sat_uplink") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	want := "answer default sbd " + imei + " " + hex.EncodeToString(answer[4:])
	if len(cc.calls) != 1 || cc.calls[0] != want {
		t.Fatalf("calls %q, want %q", cc.calls, want)
	}
	if raw := b.msgs["meshsat/"+imei+"/mo/decoded"]; raw != nil {
		t.Errorf("a frame entered the pipeline: %s", raw)
	}
}
