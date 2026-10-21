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
	"github.com/meshsat/meshsat-hub/internal/compress"
	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
)

type declared map[string]bool

func (d declared) Sealed(_ context.Context, _ string, imei string) bool { return d[imei] }

const (
	sealedIMEI = "300234065000041" // its bridge declared a sealing chain
	plainIMEI  = "300234065000042" // no declaration
)

func postMO(t *testing.T, imei string, momsn int, data []byte) MOMessage {
	t.Helper()
	b := &ackBus{}
	h := NewHandler(b, testSecret)
	h.SetEgressDeclarations(declared{sealedIMEI: true})
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
	var out MOMessage
	if err := json.Unmarshal(b.msgs["meshsat/"+imei+"/mo/decoded"], &out); err != nil {
		t.Fatal(err)
	}
	if out.Raw != base64.StdEncoding.EncodeToString(data) {
		t.Fatal("the payload is not kept as received")
	}
	return out
}

// The Rock7 (SBD) webhook decides as the Cloudloop one: plain SMAZ2 stays
// readable without a declaration (R26-R1), a declared modem's ciphertext is
// opaque with no guessed text, base64 ciphertext is opaque and keeps its text.
func TestHandler_PayloadMatrix(t *testing.T) {
	useTestKey(t)
	short := "OK at base, all safe"
	long := "Team at the north gate, all safe, moving to camp"
	v := func(b []byte) []byte { return append([]byte{codec.ProtoVersion1}, b...) }
	ctrlFree := make([]byte, 45)
	for i := range ctrlFree {
		ctrlFree[i] = byte(0x80 + (i*3)%0x80)
	}
	b64 := []byte(base64.StdEncoding.EncodeToString(make([]byte, 40)))
	key, err := hubcrypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := hubcrypto.Encrypt(key, []byte{0x00, 'o', 'n', ' ', 'm', 'y', ' ', 'w', 'a', 'y'})
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range []struct {
		name       string
		imei       string
		data       []byte
		opaque     bool
		compressed int // 1 yes, 0 no, -1 not asserted
		text       string
	}{
		{"UTF-8 text", plainIMEI, []byte("plain text from a handheld"), false, -1, "plain text from a handheld"},
		{"short SMAZ2", plainIMEI, v(compress.Compress([]byte(short))), false, 1, short},
		{"short SMAZ2, declared modem", sealedIMEI, v(compress.Compress([]byte(short))), false, 1, short},
		{"SMAZ2 over 28 bytes", plainIMEI, v(compress.Compress([]byte(long))), false, 1, long},
		{"SMAZ2 over 28 bytes, declared modem", sealedIMEI, v(compress.Compress([]byte(long))), true, 0, ""},
		{"ciphertext, declared modem", sealedIMEI, v(sealed), true, 0, ""},
		{"control-free ciphertext, declared modem", sealedIMEI, v(ctrlFree), true, 0, ""},
		{"base64 ciphertext", plainIMEI, v(b64), true, 0, string(b64)},
	} {
		out := postMO(t, c.imei, 100+i, c.data)
		if c.compressed >= 0 && out.Compressed != (c.compressed == 1) {
			t.Errorf("%s: compressed=%v", c.name, out.Compressed)
		}
		if out.Opaque != c.opaque || out.Text != c.text {
			t.Errorf("%s: opaque=%v text=%q", c.name, out.Opaque, out.Text)
		}
	}
}
