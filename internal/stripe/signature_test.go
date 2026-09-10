package stripe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const testSecret = "whsec_TESTONLYnotarealsecret"

func sign(t *testing.T, body []byte, at time.Time, secret string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%d.", at.Unix())
	mac.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", at.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

func TestAGenuineDeliveryVerifies(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"invoice.paid"}`)
	now := time.Unix(1789000000, 0).UTC()
	if err := VerifySignature(sign(t, body, now, testSecret), body, testSecret, now); err != nil {
		t.Fatalf("a genuine delivery was refused: %v", err)
	}
}

// The signature is over BYTES. Anything that changes the body must fail, which
// is why the raw body is verified before any JSON decoding: re-encoding a
// parsed body changes key order and number formatting and would break this.
func TestATamperedBodyIsRefused(t *testing.T) {
	body := []byte(`{"id":"evt_1","amount":900}`)
	now := time.Unix(1789000000, 0).UTC()
	h := sign(t, body, now, testSecret)
	for _, tampered := range []string{
		`{"id":"evt_1","amount":90000}`,
		`{"id":"evt_2","amount":900}`,
		`{"amount":900,"id":"evt_1"}`, // same JSON, different bytes
		`{"id":"evt_1","amount":900} `,
	} {
		if err := VerifySignature(h, []byte(tampered), testSecret, now); !errors.Is(err, ErrBadSignature) {
			t.Errorf("a tampered body was accepted: %s (%v)", tampered, err)
		}
	}
}

func TestTheWrongSecretIsRefused(t *testing.T) {
	body := []byte(`{"id":"evt_1"}`)
	now := time.Unix(1789000000, 0).UTC()
	h := sign(t, body, now, "whsec_SOMEBODYELSES")
	if err := VerifySignature(h, body, testSecret, now); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a delivery signed with another secret was accepted: %v", err)
	}
}

// Without a tolerance window a captured signature stays valid forever, so a
// replayed charge.refunded would keep reversing a payment.
func TestAStaleOrFutureDeliveryIsRefused(t *testing.T) {
	body := []byte(`{"id":"evt_1"}`)
	signedAt := time.Unix(1789000000, 0).UTC()
	h := sign(t, body, signedAt, testSecret)

	for _, tc := range []struct {
		skew time.Duration
		ok   bool
		why  string
	}{
		{0, true, "now"},
		{4 * time.Minute, true, "inside the window"},
		{-4 * time.Minute, true, "clock skew the other way"},
		{6 * time.Minute, false, "replayed six minutes later"},
		{72 * time.Hour, false, "replayed three days later, the end of Stripe's own retries"},
		{-6 * time.Minute, false, "dated into the future"},
	} {
		err := VerifySignature(h, body, testSecret, signedAt.Add(tc.skew))
		if tc.ok && err != nil {
			t.Errorf("%s: refused (%v)", tc.why, err)
		}
		if !tc.ok && !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: accepted", tc.why)
		}
	}
}

// Stripe sends more than one v1 while an endpoint secret is being rotated.
// Refusing the whole delivery then would drop revenue events for no gain.
func TestARotatingSecretSendsTwoSignaturesAndEitherWillDo(t *testing.T) {
	body := []byte(`{"id":"evt_1"}`)
	now := time.Unix(1789000000, 0).UTC()
	old := sign(t, body, now, "whsec_THEOLDONE")
	cur := sign(t, body, now, testSecret)
	both := old + "," + strings.SplitN(cur, ",", 2)[1] // t=…,v1=old,v1=current

	if err := VerifySignature(both, body, testSecret, now); err != nil {
		t.Fatalf("the current secret did not match during a rotation: %v", err)
	}
	if err := VerifySignature(both, body, "whsec_THEOLDONE", now); err != nil {
		t.Fatalf("the previous secret did not match during a rotation: %v", err)
	}
	if err := VerifySignature(both, body, "whsec_NEITHER", now); !errors.Is(err, ErrBadSignature) {
		t.Fatal("a third secret matched, which means the loop is not checking what it signs")
	}
}

func TestAMalformedHeaderIsRefused(t *testing.T) {
	body := []byte(`{"id":"evt_1"}`)
	now := time.Now()
	for _, h := range []string{
		"",
		"   ",
		"v1=abcdef",                  // no timestamp
		"t=1789000000",               // no signature
		"t=not-a-number,v1=abcdef",   // unreadable timestamp
		"garbage",                    // no pairs at all
		"t=1789000000,v1=zzzznothex", // not hex
		"t=1789000000,v2=abcdef",     // a scheme this does not know
	} {
		if err := VerifySignature(h, body, testSecret, now); err == nil {
			t.Errorf("a malformed header was accepted: %q", h)
		}
	}
}

// An unconfigured endpoint refuses everything rather than accepting anything.
// That is the right state for a payment endpoint nobody has set up.
func TestAnUnconfiguredEndpointRefusesEverything(t *testing.T) {
	body := []byte(`{"id":"evt_1"}`)
	now := time.Now()
	if err := VerifySignature(sign(t, body, now, ""), body, "", now); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("an unconfigured endpoint did not refuse: %v", err)
	}
}
