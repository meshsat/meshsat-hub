package rockblock

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/meshsat/meshsat-hub/internal/bridge"
	"github.com/meshsat/meshsat-hub/internal/codec"
	"github.com/meshsat/meshsat-hub/internal/compress"
	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
	"github.com/meshsat/meshsat-hub/internal/store"
)

type declared map[string]codec.Declaration

func (d declared) Declaration(_ context.Context, _ string, imei string) codec.Declaration {
	return d[imei]
}

const (
	sealedIMEI = "300234065000041" // its bridge declared a sealing chain
	plainIMEI  = "300234065000042" // no declaration
)

func postMO(t *testing.T, imei string, momsn int, data []byte) MOMessage {
	t.Helper()
	b := &ackBus{}
	h := NewHandler(b, testSecret)
	h.SetEgressDeclarations(declared{sealedIMEI: codec.DeclaredSealed})
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

// The declaration's life at the Rock7 boundary, through the real reader (as
// TestWebhook_DeclarationLifecycle on Cloudloop).
func TestHandler_DeclarationLifecycle(t *testing.T) {
	useTestKey(t)
	long := "Team at the north gate, all safe, moving to camp"
	smaz := append([]byte{codec.ProtoVersion1}, compress.Compress([]byte(long))...)
	sealedBirth := `{"bridge_id":"kit","interfaces":[{"name":"iridium_imt","imei":"` + sealedIMEI + `","egress_format":"sealed","egress_sealed":true}]}`
	noIMEIBirth := `{"bridge_id":"kit","interfaces":[{"name":"iridium_imt","imei":"","egress_format":"sealed","egress_sealed":true}]}`
	momsn := 500
	run := func(t *testing.T, d *bridge.EgressDeclarations, data []byte) MOMessage {
		t.Helper()
		momsn++
		b := &ackBus{}
		h := NewHandler(b, testSecret)
		h.SetEgressDeclarations(d)
		dataHex := hex.EncodeToString(data)
		form := url.Values{"imei": {sealedIMEI}, "momsn": {jsonInt(momsn)}, "transmit_time": {"26-10-21 10:00:00"}, "data": {dataHex}}
		form.Set("JWT", signedJWT(t, jwt.MapClaims{"imei": sealedIMEI, "momsn": momsn, "data": dataHex, "transmit_time": form.Get("transmit_time")}))
		req := httptest.NewRequest(http.MethodPost, "/api/webhook/rockblock", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		var out MOMessage
		if err := json.Unmarshal(b.msgs["meshsat/"+sealedIMEI+"/mo/decoded"], &out); err != nil {
			t.Fatal(err)
		}
		if out.Raw != base64.StdEncoding.EncodeToString(data) {
			t.Fatal("the payload is not kept as received")
		}
		return out
	}
	opaque := func(t *testing.T, out MOMessage) {
		t.Helper()
		if !out.Opaque || out.Text != "" || out.Compressed {
			t.Fatalf("opaque=%v text=%q compressed=%v", out.Opaque, out.Text, out.Compressed)
		}
	}
	readable := func(t *testing.T, out MOMessage) {
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
}
