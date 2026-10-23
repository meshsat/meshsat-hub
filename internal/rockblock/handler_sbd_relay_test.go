package rockblock

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/meshsat/meshsat-hub/internal/codec"
	"github.com/meshsat/meshsat-hub/internal/routing"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// postMORaw posts one MO and returns the mo/decoded bytes exactly as published.
func postMORaw(t *testing.T, imei string, momsn int, data []byte) []byte {
	t.Helper()
	b := &ackBus{}
	h := NewHandler(b, testSecret)
	h.SetEgressDeclarations(declared{sealedIMEI: codec.DeclaredSealed})
	dataHex := hex.EncodeToString(data)
	form := url.Values{"imei": {imei}, "momsn": {jsonInt(momsn)}, "transmit_time": {"26-10-24 02:00:00"}, "data": {dataHex}}
	form.Set("JWT", signedJWT(t, jwt.MapClaims{"imei": imei, "momsn": momsn, "data": dataHex, "transmit_time": form.Get("transmit_time")}))
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/rockblock", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	raw := b.msgs["meshsat/"+imei+"/mo/decoded"]
	if raw == nil {
		t.Fatal("nothing published on mo/decoded")
	}
	return raw
}

// sbd_mo_carries_its_wire: an SBD MO keeps the payload as the modem sent it,
// version byte included, so the relay can pass it on byte for byte. The
// declared modem's ciphertext is opaque, and a plain message carries its wire
// too.
func TestHandler_SBDMOCarriesItsWire(t *testing.T) {
	useTestKey(t)
	sealed := fixedCiphertext(t) // version byte + AES-GCM, 31 bytes
	plain := []byte("plain text from a handheld")
	for i, c := range []struct {
		name   string
		imei   string
		data   []byte
		opaque bool
	}{
		{"declared modem's ciphertext", sealedIMEI, sealed, true},
		{"plain text", plainIMEI, plain, false},
	} {
		// Read as the routing engine does: the JSON field, not the Go struct.
		var out map[string]any
		if err := json.Unmarshal(postMORaw(t, c.imei, 300+i, c.data), &out); err != nil {
			t.Fatal(err)
		}
		if out["wire"] != base64.StdEncoding.EncodeToString(c.data) {
			t.Errorf("%s: wire = %v, want the payload as sent", c.name, out["wire"])
		}
		if opaque, _ := out["opaque"].(bool); opaque != c.opaque {
			t.Errorf("%s: opaque = %v", c.name, out["opaque"])
		}
	}
}

// sbd_mo_reaches_the_relay: the bytes the Rock7 webhook publishes go through
// the real satellite_relay handler and reach the sender unchanged, on no IMT
// topic (SBD has none, so an IMT recipient gets the sender's default, RAW).
func TestHandler_SBDMOReachesTheRelay(t *testing.T) {
	useTestKey(t)
	data := fixedCiphertext(t)
	published := postMORaw(t, sealedIMEI, 310, data)

	type sent struct{ imei, wire, topic string }
	var got []sent
	relay := routing.NewSatelliteRelayHandler(func(_ context.Context, _, imei, w, topic string) error {
		got = append(got, sent{imei, w, topic})
		return nil
	})
	const far = "300258060000099" // synthetic
	relay(context.Background(), &store.Route{ID: "r", Name: "Mk2 to phone", Filter: far}, sealedIMEI, published)
	if len(got) != 1 || got[0].imei != far {
		t.Fatalf("sends = %+v, want one to %s", got, far)
	}
	wire, err := base64.StdEncoding.DecodeString(got[0].wire)
	if err != nil || string(wire) != string(data) {
		t.Errorf("relayed wire %x, want %x", wire, data)
	}
	if got[0].topic != "" {
		t.Errorf("topic %q, want none", got[0].topic)
	}
}
