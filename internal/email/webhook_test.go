package email

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func postEmail(h *WebhookHandler, target string, header map[string]string) *httptest.ResponseRecorder {
	form := url.Values{"from": {"a@example.org"}, "body": {"hi"}}
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// The inbound email webhook is auth-exempt, so the shared secret is its only
// gate: unset = refused, wrong = 401, right (header or query) = 200 (MESHSAT-976).
func TestWebhookHandler_RequiresSecret(t *testing.T) {
	h := NewWebhookHandler(nil, nil)
	if rr := postEmail(h, "/api/webhook/email", nil); rr.Code != http.StatusForbidden {
		t.Fatalf("unconfigured: got %d, want 403", rr.Code)
	}
	h.SetSecret("s3cret")
	if rr := postEmail(h, "/api/webhook/email", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing: got %d, want 401", rr.Code)
	}
	if rr := postEmail(h, "/api/webhook/email?secret=nope", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong: got %d, want 401", rr.Code)
	}
	if rr := postEmail(h, "/api/webhook/email?secret=s3cret", nil); rr.Code != http.StatusOK {
		t.Fatalf("query: got %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	if rr := postEmail(h, "/api/webhook/email", map[string]string{"X-Webhook-Secret": "s3cret"}); rr.Code != http.StatusOK {
		t.Fatalf("header: got %d, want 200", rr.Code)
	}
}
