package authentik

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// authentik returns full group objects in groups_obj, not names. Decoding that
// into the wrong shape made the whole listing fail the moment a real user
// existed, and an empty list hid it: the endpoint answered 200 with nothing in
// it right up until somebody signed up. This is a real response body.
func TestListPending_DecodesRealPayload(t *testing.T) {
	const body = `{"pagination":{"count":1},"results":[{
	  "pk":2710,"username":"probe","name":"E2E Probe","email":"probe@example.invalid",
	  "is_active":false,
	  "groups":["c4410194-0000-0000-0000-000000000000"],
	  "groups_obj":[{"pk":"c4410194-0000-0000-0000-000000000000","name":"meshsat-pending","is_superuser":false}],
	  "attributes":{"signup_ip":"203.0.113.42","organisation":"E2E","country":"NO",
	                "callsign":"E2E-1","intended_use":"verifying","email_verified":true},
	  "date_joined":"2026-09-08T21:00:00Z"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	if c == nil {
		t.Fatal("New returned nil for a configured client")
	}
	got, err := c.ListPending(context.Background())
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d users, want 1", len(got))
	}
	u := got[0]
	for _, tc := range []struct{ name, got, want string }{
		{"email", u.Email, "probe@example.invalid"},
		{"signup ip", u.SignupIP, "203.0.113.42"},
		{"organisation", u.Organisation, "E2E"},
		{"callsign", u.Callsign, "E2E-1"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	if !u.EmailVerified {
		t.Error("email_verified should decode from the attributes")
	}
	if u.Active {
		t.Error("a pending user is not active")
	}
	// The pending user must survive a round trip to the API response too.
	if _, err := json.Marshal(u); err != nil {
		t.Errorf("marshal: %v", err)
	}
}

// A blank base or token means the feature is not configured, which every
// caller reads as "say so", not "fail later".
func TestNew_UnconfiguredIsNil(t *testing.T) {
	for _, tc := range [][2]string{{"", "tok"}, {"https://auth", ""}, {"", ""}, {"  ", " "}} {
		if New(tc[0], tc[1]) != nil {
			t.Errorf("New(%q,%q) should be nil", tc[0], tc[1])
		}
	}
}

func TestValidRole(t *testing.T) {
	for _, r := range []string{"owner", "operator", "viewer"} {
		if !ValidRole(r) {
			t.Errorf("%q should be valid", r)
		}
	}
	for _, r := range []string{"", "admin", "superuser", "Owner"} {
		if ValidRole(r) {
			t.Errorf("%q should not be valid", r)
		}
	}
}
