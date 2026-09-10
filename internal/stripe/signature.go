package stripe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Verifying a delivery.
//
// This is the security boundary of the whole package. The path secret in the
// URL identifies the endpoint and keeps it off scanners; it does not
// authenticate anybody, because a URL travels through consoles, logs and
// support tickets. The signature is what says Stripe sent this.
//
// The header looks like:
//
//	Stripe-Signature: t=1699999999,v1=5257a8...,v1=...
//
// and the signed string is the timestamp, a full stop, and the RAW body --
// before any JSON decoding, because re-encoding a parsed body changes bytes
// (key order, whitespace, number formatting) and the signature is over bytes.
//
// Two failures this is written to avoid:
//
//   - Comparing hex STRINGS. hmac.Equal on the decoded bytes is the check;
//     comparing the encodings compares presentation, and a caller who sends
//     uppercase hex would be refused for the wrong reason.
//   - Accepting an old delivery. Without a tolerance window a signature
//     captured once stays valid forever, so a replay of a genuine
//     charge.refunded would keep reversing a payment. Stripe redelivers for
//     three days; a five-minute window is theirs plus generous clock skew.

// ErrBadSignature means the delivery did not come from Stripe, or did not
// arrive intact. It is deliberately one error for every reason: telling a
// caller WHICH part failed helps nobody but somebody probing the endpoint.
var ErrBadSignature = errors.New("stripe: signature does not verify")

// tolerance is how far a delivery's timestamp may be from now, in either
// direction. Future-dated deliveries are refused too: a clock that far wrong is
// a fact worth learning from a refusal rather than trusting.
const tolerance = 5 * time.Minute

// VerifySignature reports whether body was signed with secret, and whether the
// delivery is recent enough to act on.
func VerifySignature(header string, body []byte, secret string, now time.Time) error {
	if secret == "" {
		return ErrNotConfigured
	}
	ts, sigs, err := parseSignatureHeader(header)
	if err != nil {
		return err
	}
	if d := now.Sub(ts); d > tolerance || d < -tolerance {
		return fmt.Errorf("%w: timestamp is %s away from now", ErrBadSignature, d.Round(time.Second))
	}

	mac := hmac.New(sha256.New, []byte(secret))
	// The signed payload is exactly "<t>.<raw body>".
	mac.Write([]byte(strconv.FormatInt(ts.Unix(), 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	want := mac.Sum(nil)

	// Any one of the v1 signatures matching is enough: Stripe sends more than
	// one while an endpoint secret is being rotated, and refusing the whole
	// delivery during a rotation would drop revenue events.
	for _, s := range sigs {
		got, err := hex.DecodeString(s)
		if err != nil {
			continue
		}
		if hmac.Equal(got, want) {
			return nil
		}
	}
	return ErrBadSignature
}

// parseSignatureHeader pulls the timestamp and every v1 signature out of the
// header. Unknown schemes are ignored rather than refused, so a future v2 does
// not break a v1 endpoint.
func parseSignatureHeader(h string) (time.Time, []string, error) {
	var ts time.Time
	var sigs []string
	if strings.TrimSpace(h) == "" {
		return ts, nil, fmt.Errorf("%w: no signature header", ErrBadSignature)
	}
	for _, part := range strings.Split(h, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			sec, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return ts, nil, fmt.Errorf("%w: unreadable timestamp", ErrBadSignature)
			}
			ts = time.Unix(sec, 0).UTC()
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if ts.IsZero() {
		return ts, nil, fmt.Errorf("%w: no timestamp", ErrBadSignature)
	}
	if len(sigs) == 0 {
		return ts, nil, fmt.Errorf("%w: no v1 signature", ErrBadSignature)
	}
	return ts, sigs, nil
}
