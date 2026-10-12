package federation

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func signed(t *testing.T, id *Identity, to string, body []byte, now time.Time) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "https://hub.example"+RelayPath, bytes.NewReader(body))
	Sign(r, body, id, to, now)
	return r
}

func TestIdentityRoundTripAndPeerCert(t *testing.T) {
	now := time.Now()
	id, err := NewIdentity("tenant one", now)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := id.KeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	back, err := LoadIdentity(keyPEM, id.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	if back.Fingerprint != id.Fingerprint {
		t.Fatalf("fingerprint changed on reload")
	}
	fp, pub, err := ParsePeerCert(id.CertPEM)
	if err != nil || fp != id.Fingerprint || len(pub) == 0 {
		t.Fatalf("peer cert: %v %q", err, fp)
	}
	other, _ := NewIdentity("tenant two", now)
	if _, err := LoadIdentity(keyPEM, other.CertPEM); err == nil {
		t.Fatalf("a key loaded with somebody else's certificate")
	}
	if _, _, err := ParsePeerCert([]byte("not a certificate")); err == nil {
		t.Fatalf("garbage accepted as a certificate")
	}
}

func TestSignVerify(t *testing.T) {
	now := time.Now()
	a, _ := NewIdentity("a", now)
	b, _ := NewIdentity("b", now)
	_, aPub, _ := ParsePeerCert(a.CertPEM)
	body := []byte(`{"msg_id":"m1"}`)

	if err := Verify(signed(t, a, b.Fingerprint, body, now), body, aPub, now); err != nil {
		t.Fatalf("valid request refused: %v", err)
	}
	check := func(name string, r *http.Request, b []byte, want error) {
		t.Helper()
		if err := Verify(r, b, aPub, now); !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", name, err, want)
		}
	}
	// The receiver pinned A; B signs and claims to be A.
	r := signed(t, b, b.Fingerprint, body, now)
	r.Header.Set(HeaderFrom, a.Fingerprint)
	check("forged signer", r, body, ErrBadSignature)
	check("altered body", signed(t, a, b.Fingerprint, body, now), []byte(`{"msg_id":"m2"}`), ErrBadSignature)
	r = signed(t, a, b.Fingerprint, body, now)
	r.Header.Set(HeaderTo, a.Fingerprint)
	check("re-addressed", r, body, ErrBadSignature)
	r = signed(t, a, b.Fingerprint, body, now)
	r.URL.Path = "/api/other"
	check("moved path", r, body, ErrBadSignature)
	check("stale", signed(t, a, b.Fingerprint, body, now.Add(-MaxClockSkew-time.Minute)), body, ErrStale)
	check("future", signed(t, a, b.Fingerprint, body, now.Add(MaxClockSkew+time.Minute)), body, ErrStale)
	r = signed(t, a, b.Fingerprint, body, now)
	r.Header.Del(HeaderSignature)
	check("unsigned", r, body, ErrMissing)
	r = signed(t, a, b.Fingerprint, body, now)
	r.Header.Set(HeaderSignature, "AAAA")
	check("short signature", r, body, ErrBadSignature)
}

func TestRelayFrameValidate(t *testing.T) {
	ok := RelayFrame{MsgID: "mo-1:abc", Attempt: 1, FromIMEI: "300434060000001", ToIMEI: "300434060000002",
		IMTTopic: "IMT_TOPIC_RAW", WireB64: base64.StdEncoding.EncodeToString([]byte{1, 2, 3})}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid frame refused: %v", err)
	}
	cases := map[string]func(f *RelayFrame){
		"empty msg id": func(f *RelayFrame) { f.MsgID = "" },
		"attempt 0":    func(f *RelayFrame) { f.Attempt = 0 },
		"attempt 4":    func(f *RelayFrame) { f.Attempt = MaxAttempts + 1 },
		"msg id chars": func(f *RelayFrame) { f.MsgID = "a b" },
		"short imei":   func(f *RelayFrame) { f.ToIMEI = "123" },
		"plus imei":    func(f *RelayFrame) { f.FromIMEI = "+31970102582580" },
		"topic":        func(f *RelayFrame) { f.IMTTopic = "raw/../x" },
		"not base64":   func(f *RelayFrame) { f.WireB64 = "%%%" },
		"empty wire":   func(f *RelayFrame) { f.WireB64 = "" },
		"oversized wire": func(f *RelayFrame) {
			f.WireB64 = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", MaxWireBytes+1)))
		},
	}
	for name, mutate := range cases {
		f := ok
		mutate(&f)
		if err := f.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
