package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// An unknown IMT topic is refused with 400 and the accepted names, on both
// routes that can send to a 9704; a documented short name passes the check
// (here it then stops at 503: this handler has no Cloudloop sender). [MESHSAT-1669]
func TestSend_IMTTopicNames(t *testing.T) {
	h := NewSendHandler(nil, nil)
	r := chi.NewRouter()
	r.Post("/api/devices/{imei}/send", h.SendMessage)
	r.Post("/api/devices/cloudloop/{imei}/send", h.SendMessageCloudloop)

	for _, path := range []string{"/api/devices/300000000000001/send", "/api/devices/cloudloop/300000000000001/send"} {
		for _, tc := range []struct {
			body string
			want int
		}{
			{`{"text":"hi","imt_topic":"BLUE"}`, http.StatusBadRequest},
			{`{"text":"hi","ring_style":"LOUD"}`, http.StatusBadRequest},
			{`{"text":"hi","imt_topic":"RAW","ring_style":"urgent"}`, http.StatusServiceUnavailable},
			{`{"text":"hi","imt_topic":"IMT_TOPIC_RAW"}`, http.StatusServiceUnavailable},
		} {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(tc.body)))
			if rec.Code != tc.want {
				t.Errorf("%s %s = %d (%s), want %d", path, tc.body, rec.Code, strings.TrimSpace(rec.Body.String()), tc.want)
			}
			if tc.want == http.StatusBadRequest && !strings.Contains(rec.Body.String(), "use one of") {
				t.Errorf("%s %s: the 400 does not list the accepted names: %s", path, tc.body, rec.Body.String())
			}
		}
	}
}
