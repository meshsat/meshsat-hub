package sms

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type fakeClassifier struct {
	calls []string
	match bool
}

func (f *fakeClassifier) HandleInbound(_ context.Context, bearer, origin, text string) bool {
	f.calls = append(f.calls, bearer+"|"+origin+"|"+text)
	return f.match
}

// An SMS whose body is an OOB frame is handed to the classifier and stays
// out of the message pipeline (no publish); ordinary SMS still flow.
func TestWebhook_OOBFrameClassifiedBeforePipeline(t *testing.T) {
	bus := &mockBus{}
	h := NewWebhookHandler(bus, "")
	cl := &fakeClassifier{match: true}
	h.SetOOB(cl)
	post := func(body string) *httptest.ResponseRecorder {
		form := url.Values{"From": {"+31653618463"}, "To": {"+3197010258258"}, "Body": {body}, "MessageSid": {"SM1"}}
		req := httptest.NewRequest(http.MethodPost, "/api/webhook/sms", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	rr := post("MS:9W899JR000002098WTQ26XJ7V4DYYXQ28AEY1BVR")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "oob_frame") {
		t.Fatalf("frame: %d %s", rr.Code, rr.Body.String())
	}
	if len(cl.calls) != 1 || !strings.HasPrefix(cl.calls[0], "sms|+31653618463|MS:") {
		t.Fatalf("classifier calls: %v", cl.calls)
	}
	if n := len(bus.published); n != 0 {
		t.Fatalf("frame reached the pipeline: %d publishes", n)
	}
	cl.match = false
	rr = post("hello from the field")
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "oob_frame") {
		t.Fatalf("plain: %d %s", rr.Code, rr.Body.String())
	}
	if n := len(bus.published); n == 0 {
		t.Fatalf("plain SMS did not reach the pipeline")
	}
}
