package rockblock

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/golang-jwt/jwt/v5"
)

// Ground Control signs every MO webhook delivery and publishes the key to
// check it with, which is the ordinary way this is done everywhere else
// (Stripe, GitHub, Slack all sign the delivery; the shapes differ, the idea
// does not). The Hub used to compare that signed token to its own shared
// secret as though it were an opaque string, which could never match a real
// delivery, so a genuine RockBLOCK message failed verification and an invented
// one passed whenever the secret was unset.
//
// Key published at docs.groundcontrol.com/iot/rockblock/web-services (read
// 2026-09-09). It is a public key: it belongs in the repository, and pinning
// it means a delivery is checked against Ground Control's signature rather
// than against whatever a request claims about itself.
const groundControlPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAlaWAVJfNWC4XfnRx96p9
cztBcdQV6l8aKmzAlZdpEcQR6MSPzlgvihaUHNJgKm8t5ShR3jcDXIOI7er30cIN
4/9aVFMe0LWZClUGgCSLc3rrMD4FzgOJ4ibD8scVyER/sirRzf5/dswJedEiMte1
ElMQy2M6IWBACry9u12kIqG0HrhaQOzc6Tr8pHUWTKft3xwGpxCkV+K1N+9HCKFc
cbwb8okRP6FFAMm5sBbw4yAu39IVvcSL43Tucaa79FzOmfGs5mMvQfvO1ua7cOLK
fAwkhxEjirC0/RYX7Wio5yL6jmykAHJqFG2HT0uyjjrQWMtoGgwv9cIcI7xbsDX6
owIDAQAB
-----END PUBLIC KEY-----`

var (
	gcKeyOnce sync.Once
	gcKey     *rsa.PublicKey
	gcKeyErr  error
)

// verifyKey is what deliveries are checked against. It is a variable so a test
// can sign with a key pair of its own; production never changes it, because
// the whole point is that the key is pinned rather than supplied.
var verifyKey = groundControlKey

// groundControlKey parses the pinned key once.
func groundControlKey() (*rsa.PublicKey, error) {
	gcKeyOnce.Do(func() {
		block, _ := pem.Decode([]byte(groundControlPublicKeyPEM))
		if block == nil {
			gcKeyErr = fmt.Errorf("rockblock: pinned public key is not PEM")
			return
		}
		pub, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			gcKeyErr = fmt.Errorf("rockblock: pinned public key: %w", err)
			return
		}
		rsaKey, ok := pub.(*rsa.PublicKey)
		if !ok {
			gcKeyErr = fmt.Errorf("rockblock: pinned public key is %T, want RSA", pub)
			return
		}
		gcKey = rsaKey
	})
	return gcKey, gcKeyErr
}

// verifyGroundControlJWT checks the signed token Ground Control sends with an
// MO delivery, and then checks that the token describes this request.
//
// The second half is the part that matters. A signature only proves Ground
// Control produced the token; without binding it to the body, a token captured
// once could be replayed to post any payload under any IMEI. So every claim
// the token shares with the form has to agree with it.
func verifyGroundControlJWT(r *http.Request) error {
	raw := r.FormValue("JWT")
	if raw == "" {
		return fmt.Errorf("no JWT")
	}
	key, err := verifyKey()
	if err != nil {
		return err
	}
	claims := jwt.MapClaims{}
	// RS256 only: allowing the algorithm to be chosen by the token is how
	// signature checks get bypassed.
	if _, err := jwt.ParseWithClaims(raw, &claims, func(*jwt.Token) (any, error) { return key, nil },
		jwt.WithValidMethods([]string{"RS256"})); err != nil {
		return fmt.Errorf("signature: %w", err)
	}
	for _, field := range []string{"imei", "momsn", "transmit_time", "data", "serial"} {
		got := r.FormValue(field)
		want, present := claims[field]
		if !present || got == "" {
			continue
		}
		if claimString(want) != got {
			return fmt.Errorf("%s in the signed token does not match the request", field)
		}
	}
	return nil
}

// claimString renders a claim for comparison with a form value. JSON numbers
// arrive as float64, and momsn is a number in the token and a string in the
// form.
func claimString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return fmt.Sprint(t)
	}
}
