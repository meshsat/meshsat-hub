package rockblock

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

var testKey *rsa.PrivateKey

// useTestKey points verification at a key this test can sign with, for the
// duration of one test. Production always uses the pinned Ground Control key.
func useTestKey(t *testing.T) {
	t.Helper()
	if testKey == nil {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("key: %v", err)
		}
		testKey = k
	}
	prev := verifyKey
	verifyKey = func() (*rsa.PublicKey, error) { return &testKey.PublicKey, nil }
	t.Cleanup(func() { verifyKey = prev })
}

// signedJWT returns a token carrying the given claims, signed the way Ground
// Control signs a delivery.
func signedJWT(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(testKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func moRequest(form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/rockblock", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// A delivery Ground Control signed is accepted; the same token with the body
// changed underneath it is not. The second half is the point: a signature only
// says who made the token, and without binding it to the request a token seen
// once could be replayed to post anything under any IMEI.
func TestGroundControlJWT(t *testing.T) {
	useTestKey(t)
	const imei = "300234065123456"
	tok := signedJWT(t, jwt.MapClaims{"imei": imei, "momsn": 42, "data": "48656c6c6f"})

	t.Run("signed delivery is accepted", func(t *testing.T) {
		r := moRequest(url.Values{"imei": {imei}, "momsn": {"42"}, "data": {"48656c6c6f"}, "JWT": {tok}})
		if err := verifyGroundControlJWT(r); err != nil {
			t.Errorf("a correctly signed delivery was rejected: %v", err)
		}
	})

	t.Run("replayed onto another device is refused", func(t *testing.T) {
		r := moRequest(url.Values{"imei": {"300234065999999"}, "momsn": {"42"}, "data": {"48656c6c6f"}, "JWT": {tok}})
		if err := verifyGroundControlJWT(r); err == nil {
			t.Error("a token from one device was accepted for another")
		}
	})

	t.Run("payload swapped under the signature is refused", func(t *testing.T) {
		r := moRequest(url.Values{"imei": {imei}, "momsn": {"42"}, "data": {"deadbeef"}, "JWT": {tok}})
		if err := verifyGroundControlJWT(r); err == nil {
			t.Error("a token was accepted for a payload it did not cover")
		}
	})

	t.Run("signed by somebody else is refused", func(t *testing.T) {
		other, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("key: %v", err)
		}
		bad, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"imei": imei}).SignedString(other)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		r := moRequest(url.Values{"imei": {imei}, "JWT": {bad}})
		if err := verifyGroundControlJWT(r); err == nil {
			t.Error("a token signed by the wrong key was accepted")
		}
	})

	t.Run("unsigned token is refused", func(t *testing.T) {
		none, err := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"imei": imei}).
			SignedString(jwt.UnsafeAllowNoneSignatureType)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		r := moRequest(url.Values{"imei": {imei}, "JWT": {none}})
		if err := verifyGroundControlJWT(r); err == nil {
			t.Error("alg=none was accepted; the algorithm must not be the token's to choose")
		}
	})

	t.Run("no token at all", func(t *testing.T) {
		if err := verifyGroundControlJWT(moRequest(url.Values{"imei": {imei}})); err == nil {
			t.Error("a request with no token was accepted")
		}
	})
}

// The key shipped in the source must be the RSA public key Ground Control
// publishes, and must parse.
func TestPinnedKeyParses(t *testing.T) {
	k, err := groundControlKey()
	if err != nil {
		t.Fatalf("pinned key: %v", err)
	}
	if k.Size() != 256 {
		t.Errorf("pinned key is %d bits, want 2048", k.Size()*8)
	}
}
