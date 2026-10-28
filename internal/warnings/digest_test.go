package warnings

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/geo"
)

// MESHSAT-1734. The test key and the vector below are the ones in
// docs/warning-relay.md; the Bridge's decoder test reuses them, so a change
// here is a change of the contract.

// testKeyD is the P-256 private scalar of the test key (never a real key).
const testKeyD = "1f2d3c4b5a69788796a5b4c3d2e1f00112233445566778899aabbccddeeff001"

// testKeyPKIXHex is its public key, PKIX DER.
const testKeyPKIXHex = "3059301306072a8648ce3d020106082a8648ce3d03010703420004" +
	"8255ccf3e18d398e17a3aa927965387335c09fcea6e05c7671f134b2f6b4a799" +
	"588325ca016280e097d515e21218b6528bf1b7cd8ddde016baee3cecc2c95445"

// vectorHex is the digest of the vector warning, signed by the test key.
// Fixed time 2026-10-28T10:11:41Z, expires 2026-10-29T10:11:41Z.
const vectorHex = "" +
	"57010102f161d743dd14e97400000000000000006ae1ca5d6ae31bdd2241016e6c11024e4c383034000000000b4b857320e820dc918cbb8d" + // the body, 56 bytes
	"f76c877ecedb20f0442bf905ec8e55b41bc8a7701b47892201770a5e189e163e" + // r
	"2d81e77d1bfe52e766d5c90c2748cdcc0d31ccd360eb731e8e2fc75b4eba865b" // s

type testSigner struct{ key *ecdsa.PrivateKey }

func newTestSigner(t *testing.T) *testSigner {
	t.Helper()
	d, ok := new(big.Int).SetString(testKeyD, 16)
	if !ok {
		t.Fatal("test key")
	}
	key := &ecdsa.PrivateKey{D: d, PublicKey: ecdsa.PublicKey{Curve: elliptic.P256()}}
	key.PublicKey.X, key.PublicKey.Y = key.PublicKey.Curve.ScalarBaseMult(d.Bytes())
	return &testSigner{key: key}
}

func (s *testSigner) Sign(msg []byte) ([]byte, error) {
	h := sha256.Sum256(msg)
	return ecdsa.SignASN1(rand.Reader, s.key, h[:])
}

func (s *testSigner) PublicKey() []byte {
	der, _ := x509.MarshalPKIXPublicKey(&s.key.PublicKey)
	return der
}

func vectorDigest() *Digest {
	return &Digest{
		MsgType: TypeAlert, Source: SourceMeteoAlarmNL,
		ID:   IDOf("2.49.0.0.528.0.NL.20261028101141.6ddd5a99-421a-4c94-b9ea-25fa0ddcdd48_2"),
		Sent: time.Date(2026, 10, 28, 10, 11, 41, 0, time.UTC), Expires: time.Date(2026, 10, 29, 10, 11, 41, 0, time.UTC),
		Severity: 2, Urgency: 2, Certainty: 4, Category: 1, Event: EventWind, Language: "nl",
		Geocodes: []Geocode{{Type: GeocodeEMMAID, Value: "NL804"}},
		Text:     "Kans op windstoten",
	}
}

func TestTestKeyIsTheDocumentedOne(t *testing.T) {
	s := newTestSigner(t)
	if got := hex.EncodeToString(s.PublicKey()); got != testKeyPKIXHex {
		t.Fatalf("the test key's public key is %s, the documented one is %s", got, testKeyPKIXHex)
	}
}

// TestDocumentedVector holds the bytes docs/warning-relay.md shows: the body
// is what Encode writes for the vector warning, and the signature verifies
// with the test key.
func TestDocumentedVector(t *testing.T) {
	wire, err := hex.DecodeString(vectorHex)
	if err != nil {
		t.Fatal(err)
	}
	body, err := encodeBody(vectorDigest())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wire[:len(wire)-SignatureBytes], body) {
		t.Fatalf("the vector's body is not what Encode writes:\n vector %x\n encode %x", wire[:len(wire)-SignatureBytes], body)
	}
	pub, _ := hex.DecodeString(testKeyPKIXHex)
	d, err := Verify(pub, wire)
	if err != nil {
		t.Fatalf("the vector does not verify: %v", err)
	}
	if d.Text != "Kans op windstoten" || d.Source != SourceMeteoAlarmNL || d.Language != "nl" || d.Event != EventWind ||
		len(d.Geocodes) != 1 || d.Geocodes[0].Value != "NL804" || d.Geocodes[0].Type != GeocodeEMMAID ||
		!d.Sent.Equal(vectorDigest().Sent) || !d.Expires.Equal(vectorDigest().Expires) || d.Severity != 2 || d.Certainty != 4 {
		t.Fatalf("decoded: %+v", d)
	}
	// One flipped byte in the body: refused.
	bad := append([]byte(nil), wire...)
	bad[40] ^= 0x01
	if _, err := Verify(pub, bad); err == nil {
		t.Fatal("a changed digest verified")
	}
	// One flipped byte in the signature: refused.
	bad = append([]byte(nil), wire...)
	bad[len(bad)-1] ^= 0x01
	if _, err := Verify(pub, bad); err == nil {
		t.Fatal("a changed signature verified")
	}
	// Another key: refused.
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	otherPub, _ := x509.MarshalPKIXPublicKey(&other.PublicKey)
	if _, err := Verify(otherPub, wire); err == nil {
		t.Fatal("verified under another key")
	}
}

// TestPrintVector regenerates the vector with the test key; run it with
// WARN_VECTOR=1 when the layout changes and paste the output here and into
// docs/warning-relay.md.
func TestPrintVector(t *testing.T) {
	if os.Getenv("WARN_VECTOR") == "" {
		t.Skip("WARN_VECTOR not set")
	}
	wire, err := Encode(vectorDigest(), newTestSigner(t))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("vector (%d bytes): %x\n", len(wire), wire)
}

func TestRoundTripAndSizeBound(t *testing.T) {
	s := newTestSigner(t)
	long := strings.Repeat("Zware windstoten tot 100 km/u aan de kust en boven het IJsselmeer. ", 5)
	cases := []struct {
		name string
		d    *Digest
	}{
		{"vector", vectorDigest()},
		{"eight geocodes and a long text", &Digest{MsgType: TypeUpdate, Test: true, Source: SourceDWD,
			ID: IDOf("a"), Ref: IDOf("b"), Sent: time.Unix(1793187780, 0), Severity: 4, Urgency: 3, Certainty: 3, Category: 1,
			Event: EventThunderstorm, Language: "de", Text: long,
			Geocodes: []Geocode{{1, "501000005"}, {1, "501000001"}, {1, "501000002"}, {1, "501000003"}, {1, "105758000"}, {1, "805758000"}, {1, "905758000"}, {1, "705758000"}, {1, "dropped"}}}},
		{"a polygon of forty points", &Digest{MsgType: TypeCancel, Source: SourceMeteoAlarmNL, ID: IDOf("c"), Ref: IDOf("d"),
			Sent: time.Unix(1793189000, 0), Event: EventWind, Language: "en", Text: "Cancelled", Polygon: texelPolygon()}},
		{"nothing but the header", &Digest{MsgType: TypeAlert, Source: SourceTest, Sent: time.Unix(1, 0)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wire, err := Encode(c.d, s)
			if err != nil {
				t.Fatal(err)
			}
			if len(wire) > MaxDigestBytes || len(wire) < MinDigestBytes {
				t.Fatalf("%d bytes", len(wire))
			}
			got, err := Verify(s.PublicKey(), wire)
			if err != nil {
				t.Fatal(err)
			}
			if got.MsgType != c.d.MsgType || got.Test != c.d.Test || got.Source != c.d.Source || got.ID != c.d.ID || got.Ref != c.d.Ref ||
				!got.Sent.Equal(c.d.Sent.UTC()) || got.Event != c.d.Event || got.Severity != c.d.Severity {
				t.Fatalf("round trip: %+v", got)
			}
			if c.d.Language != "" && got.Language != c.d.Language {
				t.Fatalf("language %q", got.Language)
			}
			if len(c.d.Geocodes) > 8 {
				// The ninth is dropped, the first eight kept in order.
				if len(got.Geocodes) != 8 || got.Geocodes[0].Value != "501000005" || got.Geocodes[7].Value != "705758000" {
					t.Fatalf("geocodes: %+v", got.Geocodes)
				}
			} else if len(got.Geocodes) != len(c.d.Geocodes) {
				t.Fatalf("geocodes: %+v", got.Geocodes)
			}
			if len(c.d.Polygon) > 0 {
				if len(got.Polygon) < 3 || len(got.Polygon) > 12 {
					t.Fatalf("polygon: %d points", len(got.Polygon))
				}
				// Within 11 m of a simplified vertex.
				for _, p := range got.Polygon {
					best := 1e9
					for _, q := range c.d.Polygon {
						if d := geo.HaversineDistance(p, q); d < best {
							best = d
						}
					}
					if best > 12 {
						t.Fatalf("a polygon point moved %.0f m", best)
					}
				}
			}
			if c.d.Text != "" && (got.Text == "" || !strings.HasPrefix(c.d.Text, got.Text)) {
				t.Fatalf("text %q is not a prefix of the original", got.Text)
			}
			if c.name == "eight geocodes and a long text" && len(got.Text) > 120 {
				t.Fatalf("the text was not shortened to fit: %d chars", len(got.Text))
			}
		})
	}
}

func TestDecodeRefusals(t *testing.T) {
	s := newTestSigner(t)
	wire, _ := Encode(vectorDigest(), s)
	cases := []struct {
		name string
		mut  func([]byte) []byte
	}{
		{"short", func(b []byte) []byte { return b[:MinDigestBytes-1] }},
		{"long", func(b []byte) []byte { return append(b, make([]byte, MaxDigestBytes)...) }},
		{"magic", func(b []byte) []byte { b[0] = 0x01; return b }},
		{"version", func(b []byte) []byte { b[1] = 0x02; return b }},
		{"message type 0", func(b []byte) []byte { b[2] = 0; return b }},
		{"area kind 3", func(b []byte) []byte { b[33] = 0x31; return b }},
		{"text longer than the body", func(b []byte) []byte { b[34+geocodeBytes] = 200; return b }},
	}
	for _, c := range cases {
		b := c.mut(append([]byte(nil), wire...))
		if _, _, err := Decode(b); err == nil {
			t.Errorf("%s: decoded", c.name)
		}
	}
	if _, err := Encode(&Digest{MsgType: 7, Source: 1}, s); err == nil {
		t.Error("message type 7 encoded")
	}
}

func TestFromAlertTakesTheTenantsLanguageAndReferences(t *testing.T) {
	a, err := ParseCAP(fixture(t, "meteoalarm-nl-texel.cap.xml"))
	if err != nil {
		t.Fatal(err)
	}
	d := FromAlert(a, SourceMeteoAlarmNL, []string{"nl", "en"})
	if d.MsgType != TypeUpdate || d.Ref != IDOf("2.49.0.0.528.0.NL.20261028041400.6ddd5a99-421a-4c94-b9ea-25fa0ddcdd48_1") || d.ID != IDOf(a.Identifier) {
		t.Fatalf("ids: %+v", d)
	}
	if d.Language != "nl-NL" || !strings.Contains(strings.ToLower(d.Text), "wind") || d.Event != EventWind || d.Severity != 2 {
		t.Fatalf("content: %+v", d)
	}
	if len(d.Geocodes) != 1 || d.Geocodes[0] != (Geocode{GeocodeEMMAID, "NL804"}) || len(d.Polygon) != 0 {
		t.Fatalf("area: geocodes win over the polygon: %+v", d)
	}
	if d.Test {
		t.Fatal("an Actual message marked as a test")
	}
	wire, err := Encode(d, newTestSigner(t))
	if err != nil || len(wire) > MaxDigestBytes {
		t.Fatalf("encode: %d %v", len(wire), err)
	}
	// A message with no geocode carries its polygon.
	for i := range a.Infos {
		for j := range a.Infos[i].Areas {
			a.Infos[i].Areas[j].Geocodes = nil
		}
	}
	d = FromAlert(a, SourceMeteoAlarmNL, nil)
	if len(d.Polygon) < 30 || len(d.Geocodes) != 0 {
		t.Fatalf("polygon: %d points", len(d.Polygon))
	}
	wire, err = Encode(d, newTestSigner(t))
	if err != nil || len(wire) > MaxDigestBytes {
		t.Fatalf("encode with polygon: %d %v", len(wire), err)
	}
	got, _, _ := Decode(wire)
	if len(got.Polygon) < 3 || len(got.Polygon) > 12 {
		t.Fatalf("decoded polygon: %d", len(got.Polygon))
	}
}

func TestEventFromWords(t *testing.T) {
	cases := map[string]int{
		"Amtliche Warnung vor BÖEN": EventWind, "Sneeuw en ijzel": EventSnowIce, "onweersbuien": EventThunderstorm,
		"Dichte mist": EventFog, "Hitte": EventHeat, "Hochwasser": EventFlood, "tornado warning": EventTornado, "nothing known": EventOther,
	}
	for s, want := range cases {
		if got := eventFromWords(s); got != want {
			t.Errorf("%q: %d, want %d", s, got, want)
		}
	}
}

// texelPolygon is the ring of the MeteoAlarm fixture.
func texelPolygon() []geo.Point {
	return parsePolygon("52.99670,4.75700 52.97000,4.75590 52.96800,4.73510 52.96530,4.72380 52.95250,4.71660 52.88040,4.70670 52.81580,4.68160 52.76700,4.64280 52.80430,3.80980 53.27920,3.98250 53.51030,4.15500 53.79500,4.57900 54.04220,5.47990 53.40710,5.37810 53.38700,5.34340 53.37400,5.29390 53.37420,5.26230 53.36830,5.22720 53.34380,5.18400 53.34990,5.15860 53.30760,5.09710 53.29670,5.09540 53.29650,5.06910 53.28250,5.03560 53.26710,4.97030 53.25330,4.98390 53.23570,4.93950 53.21630,4.91840 53.21950,4.88740 53.18820,4.85420 53.16020,4.87900 53.14170,4.90250 53.08080,4.89390 53.03290,4.84800 53.02970,4.81880 53.00290,4.78910 52.99670,4.75700")
}
