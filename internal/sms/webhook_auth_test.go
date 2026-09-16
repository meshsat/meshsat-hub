package sms

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// relayHMAC is the custom-relay signature: HMAC-SHA256 over From+Body, hex.
// It is reproduced here rather than exported from the handler so a change to
// the production code cannot quietly redefine what the test asserts.
func relayHMAC(secret, from, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(from + body))
	return hex.EncodeToString(mac.Sum(nil))
}

// testAuthToken stands in for a Twilio ACCOUNT auth token.
const testAuthToken = "account-auth-token"

// signedRequest builds the inbound POST a real Twilio delivery makes: form
// encoded, with a valid X-Twilio-Signature over the URL and the parameters.
//
// httptest.NewRequest sets Host to example.com and leaves TLS nil, so the URL
// the handler reconstructs is http://example.com + the request URI.
func signedRequest(t *testing.T, form url.Values) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/sms", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature",
		twilioSignature(testAuthToken, "http://example.com/api/webhook/sms", form))
	return req
}

func inboundForm() url.Values {
	return url.Values{
		"From":       {"+31612345678"},
		"To":         {"+31698765432"},
		"Body":       {"Hello from phone"},
		"MessageSid": {"SM999"},
	}
}

// TestTwilioSignatureConstruction pins the algorithm, which is the part that is
// easy to get subtly wrong and impossible to notice: Twilio's scheme is the full
// URL, then every POST parameter as name immediately followed by value with the
// names sorted, HMAC-SHA1 under the auth token, base64.
//
// Both the signed string and the resulting digest are asserted. The digest was
// cross-checked against an independent implementation of the same documented
// algorithm rather than copied from a vendor page, so it pins OUR construction;
// the signed string below is what makes the construction itself readable, and a
// drift in either shows up here instead of as refused satellite traffic.
func TestTwilioSignatureConstruction(t *testing.T) {
	const (
		u       = "https://mycompany.com/myapp.php?foo=1&bar=2"
		tok     = "12345"
		wantStr = "https://mycompany.com/myapp.php?foo=1&bar=2" +
			"CallSidCA1234567890ABCDE" + "Caller+14158675310" + "Digits1234" +
			"From+14158675310" + "To+18005551212"
		wantSig = "GvWf1cFY/Q7PnoempGyD5oXAezc="
	)
	form := url.Values{
		"Digits":  {"1234"},
		"To":      {"+18005551212"},
		"From":    {"+14158675310"},
		"Caller":  {"+14158675310"},
		"CallSid": {"CA1234567890ABCDE"},
	}
	if got := signedString(u, form); got != wantStr {
		t.Errorf("signed string =\n%q\nwant\n%q", got, wantStr)
	}
	if got := twilioSignature(tok, u, form); got != wantSig {
		t.Errorf("twilioSignature = %q, want %q", got, wantSig)
	}
}

// TestUnauthenticatedInboundIsRefused is the regression for MESHSAT-1168.
//
// With no credential configured the handler used to skip the check entirely and
// accept the message, which put anonymous message injection into dedup, fragment
// reassembly, the dead man's switch, the OOB command service and the audit log.
// Its sibling webhooks (email, globalstar) have always refused this. A 200 here
// means the hole is back.
func TestUnauthenticatedInboundIsRefused(t *testing.T) {
	h := NewWebhookHandler(&mockBus{}, "") // no relay secret, no auth token

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/sms",
		strings.NewReader(inboundForm().Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("an unauthenticated inbound SMS got %d, want 403: %s", w.Code, w.Body.String())
	}
}

// A message must not reach the pipeline when it is refused. Checking the status
// alone would pass even if the handler answered 403 after publishing.
func TestRefusedInboundPublishesNothing(t *testing.T) {
	mb := &mockBus{}
	h := NewWebhookHandler(mb, "")

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/sms",
		strings.NewReader(inboundForm().Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if len(mb.published) != 0 {
		t.Fatalf("a refused SMS published %d messages, want 0", len(mb.published))
	}
}

func TestValidTwilioSignatureIsAccepted(t *testing.T) {
	mb := &mockBus{}
	h := NewWebhookHandler(mb, "")
	h.SetInboundAuthToken(testAuthToken)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, signedRequest(t, inboundForm()))

	if w.Code != http.StatusOK {
		t.Fatalf("a correctly signed SMS got %d, want 200: %s", w.Code, w.Body.String())
	}
	if len(mb.published) == 0 {
		t.Error("a correctly signed SMS published nothing")
	}
}

func TestWrongTwilioSignatureIsRefused(t *testing.T) {
	h := NewWebhookHandler(&mockBus{}, "")
	h.SetInboundAuthToken(testAuthToken)

	req := signedRequest(t, inboundForm())
	req.Header.Set("X-Twilio-Signature", "Zm9yZ2VkIHNpZ25hdHVyZSBoZXJl")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("a forged signature got %d, want 401", w.Code)
	}
}

func TestMissingTwilioSignatureIsRefused(t *testing.T) {
	h := NewWebhookHandler(&mockBus{}, "")
	h.SetInboundAuthToken(testAuthToken)

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/sms",
		strings.NewReader(inboundForm().Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("a request with no signature got %d, want 401", w.Code)
	}
}

// The signature must cover the whole message, not just From and Body. The old
// home-grown MAC covered only those two, so To and MessageSid could be rewritten
// under a captured signature.
func TestSignatureCoversEveryParameter(t *testing.T) {
	for _, field := range []string{"To", "MessageSid", "Body", "From"} {
		t.Run(field, func(t *testing.T) {
			form := inboundForm() // the signature is computed over this
			tampered := inboundForm()
			tampered.Set(field, "tampered+1")

			req := httptest.NewRequest(http.MethodPost, "/api/webhook/sms",
				strings.NewReader(tampered.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("X-Twilio-Signature",
				twilioSignature(testAuthToken, "http://example.com/api/webhook/sms", form))

			h := NewWebhookHandler(&mockBus{}, "")
			h.SetInboundAuthToken(testAuthToken)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("tampering with %s was accepted (%d), want 401", field, w.Code)
			}
		})
	}
}

// The custom-relay HMAC path stays working for the relays already using it.
func TestCustomRelaySecretStillAuthenticates(t *testing.T) {
	h := NewWebhookHandler(&mockBus{}, "secret123")

	form := url.Values{"From": {"+31612345678"}, "Body": {"hi"}}
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/sms", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Signature", relayHMAC("secret123", "+31612345678", "hi"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("a valid custom-relay signature got %d, want 200: %s", w.Code, w.Body.String())
	}
}

// A Twilio auth token takes precedence over the weaker relay secret: when both
// are set, a relay-style signature alone must not get in.
func TestAuthTokenTakesPrecedenceOverRelaySecret(t *testing.T) {
	h := NewWebhookHandler(&mockBus{}, "secret123")
	h.SetInboundAuthToken(testAuthToken)

	form := url.Values{"From": {"+31612345678"}, "Body": {"hi"}}
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/sms", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Signature", relayHMAC("secret123", "+31612345678", "hi"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("the weaker relay signature was accepted (%d) while an auth token was configured", w.Code)
	}
}
