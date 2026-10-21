package rockblock

import (
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
	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
)

func postMO(t *testing.T, h *Handler, imei string, momsn int, data []byte) {
	t.Helper()
	dataHex := hex.EncodeToString(data)
	form := url.Values{"imei": {imei}, "momsn": {jsonInt(momsn)}, "transmit_time": {"26-10-21 10:00:00"}, "data": {dataHex}}
	form.Set("JWT", signedJWT(t, jwt.MapClaims{"imei": imei, "momsn": momsn, "data": dataHex, "transmit_time": form.Get("transmit_time")}))
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/rockblock", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

// An SBD message from a kit whose chain dropped base64 (HeMB-NG S3-2) is
// ciphertext for a key the Hub does not hold: opaque, no text, never a SMAZ2
// guess. Text from a device without a chain is still read.
func TestHandler_KitCiphertextWithoutBase64IsOpaque(t *testing.T) {
	useTestKey(t)
	key, err := hubcrypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	const imei = "300234065000031"
	for momsn, n := range []int{1, 2, 20, 100, 311} {
		frame := make([]byte, n)
		for i := range frame {
			frame[i] = byte(i*7 + n)
		}
		sealed, err := hubcrypto.Encrypt(key, frame)
		if err != nil {
			t.Fatal(err)
		}
		b := &ackBus{}
		h := NewHandler(b, testSecret)
		postMO(t, h, imei, momsn+1, append([]byte{codec.ProtoVersion1}, sealed...))
		var out MOMessage
		if err := json.Unmarshal(b.msgs["meshsat/"+imei+"/mo/decoded"], &out); err != nil {
			t.Fatal(err)
		}
		if !out.Opaque || out.Text != "" || out.Compressed {
			t.Fatalf("%d-byte frame: opaque=%v text=%q compressed=%v", n, out.Opaque, out.Text, out.Compressed)
		}
	}
	// Built so the old SMAZ2 guess reads it as letters (bigram codes, no
	// control byte), and the base64 form, which keeps its text.
	bigrams := []byte{codec.ProtoVersion1}
	for i := 0; i < 60; i++ {
		bigrams = append(bigrams, byte(0x80+i))
	}
	b64 := append([]byte{codec.ProtoVersion1}, []byte(base64.StdEncoding.EncodeToString(make([]byte, 40)))...)
	for momsn, c := range []struct {
		name string
		data []byte
		text string
	}{{"bigram-shaped", bigrams, ""}, {"base64", b64, string(b64[1:])}} {
		b := &ackBus{}
		h := NewHandler(b, testSecret)
		postMO(t, h, imei, 20+momsn, c.data)
		var out MOMessage
		if err := json.Unmarshal(b.msgs["meshsat/"+imei+"/mo/decoded"], &out); err != nil {
			t.Fatal(err)
		}
		if !out.Opaque || out.Text != c.text {
			t.Fatalf("%s: opaque=%v text=%q", c.name, out.Opaque, out.Text)
		}
	}
	b := &ackBus{}
	h := NewHandler(b, testSecret)
	postMO(t, h, imei, 9, []byte("plain text from a handheld"))
	var out MOMessage
	if err := json.Unmarshal(b.msgs["meshsat/"+imei+"/mo/decoded"], &out); err != nil {
		t.Fatal(err)
	}
	if out.Opaque || out.Text != "plain text from a handheld" {
		t.Fatalf("plain text: opaque=%v text=%q", out.Opaque, out.Text)
	}
}
