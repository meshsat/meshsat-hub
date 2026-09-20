package sms

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func statusForm() url.Values {
	return url.Values{
		"MessageSid":    {"SM123"},
		"MessageStatus": {"delivered"},
		"To":            {"whatsapp:+31612345678"},
		"From":          {"whatsapp:+3197000000001"},
	}
}

func statusRequest(t *testing.T, form url.Values, sign bool) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/whatsapp/status",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if sign {
		req.Header.Set("X-Twilio-Signature",
			twilioSignature(testAuthToken, "http://example.com/api/webhook/whatsapp/status", form))
	}
	return req
}

// The status callback sits under /api/webhook/, which is auth-exempt by prefix,
// so the Twilio signature is the only thing in front of it. Unsigned means
// anyone can assert that a message was delivered -- or that it failed.
func TestStatusCallbackRefusesUnsigned(t *testing.T) {
	h := NewStatusHandler("whatsapp", testAuthToken, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, statusRequest(t, statusForm(), false))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned delivery receipt got %d, want 401", w.Code)
	}
}

func TestStatusCallbackRefusesForgedSignature(t *testing.T) {
	h := NewStatusHandler("whatsapp", testAuthToken, "")
	req := statusRequest(t, statusForm(), false)
	req.Header.Set("X-Twilio-Signature", "Zm9yZ2VkIHNpZ25hdHVyZQ==")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("forged signature got %d, want 401", w.Code)
	}
}

func TestStatusCallbackAcceptsSigned(t *testing.T) {
	h := NewStatusHandler("whatsapp", testAuthToken, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, statusRequest(t, statusForm(), true))

	if w.Code != http.StatusNoContent {
		t.Fatalf("a correctly signed receipt got %d, want 204: %s", w.Code, w.Body.String())
	}
}

// With no account auth token there is no way to authenticate a receipt, so it
// must refuse rather than trust the caller. A relay secret does not help here:
// Twilio is the only thing that sends these and it signs them.
func TestStatusCallbackRefusesWhenNoAuthTokenConfigured(t *testing.T) {
	for _, tc := range []struct{ name, token, secret string }{
		{"nothing configured", "", ""},
		{"only a relay secret", "", "relay-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewStatusHandler("whatsapp", tc.token, tc.secret)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, statusRequest(t, statusForm(), false))
			if w.Code != http.StatusForbidden {
				t.Fatalf("got %d, want 403", w.Code)
			}
		})
	}
}

func TestStatusCallbackRejectsGet(t *testing.T) {
	h := NewStatusHandler("whatsapp", testAuthToken, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/webhook/whatsapp/status", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET got %d, want 405", w.Code)
	}
}
