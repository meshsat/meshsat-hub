package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/authentik"
	"github.com/meshsat/meshsat-hub/internal/mail"
)

type recordingSender struct {
	mu   sync.Mutex
	sent []string
}

func (r *recordingSender) SendMessage(_ context.Context, to string, _ mail.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, to)
	return nil
}

// A minimal authentik: one pending user, groups by name, PATCH accepted.
func fakeAuthentik(t *testing.T, attrs map[string]any) *authentik.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/core/groups/", func(w http.ResponseWriter, r *http.Request) {
		pk := map[string]string{"meshsat-pending": "g-pending", "meshsat-owner": "g-owner"}[r.URL.Query().Get("name")]
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{{"pk": pk}}})
	})
	mux.HandleFunc("/api/v3/core/users/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"pk": 7, "username": "journey-0a1b2c3d", "email": "journey-0a1b2c3d@meshsat.org", "name": "Journey Probe",
				"is_active": false, "groups": []string{"g-pending"}, "attributes": attrs})
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return authentik.New(srv.URL, "tok")
}

// Five "your account is ready" emails reached the operator's inbox on the day
// the nightly verification job was built, one per run: the probe's address is
// a catch-all. A probe is approved without being written to; a customer still is.
func TestApprovingAVerificationProbeSendsNoEmail(t *testing.T) {
	for _, tc := range []struct {
		name  string
		attrs map[string]any
		mails int
	}{
		{"probe", map[string]any{"email_verified": true, "verification_probe": true}, 0},
		{"customer", map[string]any{"email_verified": true}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewSignupHandler(fakeAuthentik(t, tc.attrs), nil)
			rec := &recordingSender{}
			h.SetMailer(rec, "https://hub.example.net")
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/admin/signups/7/approve", strings.NewReader(`{"role":"owner"}`))
			req.Header.Set("Content-Type", "application/json")
			signupRouter(h).ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("approve: %d %s", w.Code, w.Body.String())
			}
			if len(rec.sent) != tc.mails {
				t.Fatalf("%s: %d email(s) sent, want %d (to %v)", tc.name, len(rec.sent), tc.mails, rec.sent)
			}
		})
	}
}
