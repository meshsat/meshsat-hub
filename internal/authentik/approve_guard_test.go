package authentik

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeAK serves just enough authentik for Approve/Reject: one user and the two
// groups they are moved between. It records every PATCH and DELETE so a test
// can assert that a refusal really wrote nothing.
type fakeAK struct {
	user    map[string]any
	patches []map[string]any
	deletes int
}

func (f *fakeAK) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/core/groups/", func(w http.ResponseWriter, r *http.Request) {
		pk := map[string]string{"meshsat-pending": "g-pending", "meshsat-owner": "g-owner"}[r.URL.Query().Get("name")]
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{{"pk": pk}}})
	})
	mux.HandleFunc("/api/v3/core/users/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(f.user)
		case http.MethodPatch:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.patches = append(f.patches, body)
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			f.deletes++
			w.WriteHeader(http.StatusNoContent)
		}
	})
	return mux
}

func newFake(t *testing.T, user map[string]any) (*Client, *fakeAK) {
	t.Helper()
	f := &fakeAK{user: user}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return New(srv.URL, "tok"), f
}

func pendingUser() map[string]any {
	return map[string]any{
		"pk": 7, "username": "alice", "email": "alice@example.com",
		"is_active": false, "groups": []string{"g-pending"},
		"attributes": map[string]any{"email_verified": true, "signup_ip": "203.0.113.9"},
	}
}

func TestApprove_HappyPathActivatesAndMovesGroup(t *testing.T) {
	c, f := newFake(t, pendingUser())
	ip, email, err := c.Approve(context.Background(), 7, "owner")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if ip != "203.0.113.9" || email != "alice@example.com" {
		t.Fatalf("returned %q / %q", ip, email)
	}
	if len(f.patches) != 1 {
		t.Fatalf("want 1 patch, got %d", len(f.patches))
	}
	p := f.patches[0]
	if p["is_active"] != true {
		t.Errorf("is_active not set true: %v", p["is_active"])
	}
	groups, _ := p["groups"].([]any)
	if len(groups) != 1 || groups[0] != "g-owner" {
		t.Errorf("groups = %v, want just the role group", groups)
	}
}

// The reason this guard exists: the Hub's authentik identity is an admin on an
// instance shared with another product, so an unguarded Approve would hand any
// account in it an active session and the owner role, from a pk in the URL.
func TestApprove_RefusesAnAccountThatIsNotAPendingSignup(t *testing.T) {
	for _, tc := range []struct {
		name string
		user map[string]any
	}{
		{"already active", map[string]any{"pk": 7, "email": "someone@omoikane.example", "is_active": true,
			"groups": []string{"g-pending"}, "attributes": map[string]any{"email_verified": true}}},
		{"never pending", map[string]any{"pk": 7, "email": "someone@omoikane.example", "is_active": false,
			"groups": []string{"some-other-group"}, "attributes": map[string]any{"email_verified": true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f := newFake(t, tc.user)
			if _, _, err := c.Approve(context.Background(), 7, "owner"); !errors.Is(err, ErrNotPending) {
				t.Fatalf("err = %v, want ErrNotPending", err)
			}
			if len(f.patches) != 0 {
				t.Fatalf("refused approval still wrote %d patch(es)", len(f.patches))
			}
		})
	}
}

func TestApprove_RefusesUnverifiedEmail(t *testing.T) {
	u := pendingUser()
	u["attributes"] = map[string]any{"email_verified": false}
	c, f := newFake(t, u)
	if _, _, err := c.Approve(context.Background(), 7, "owner"); !errors.Is(err, ErrEmailNotVerified) {
		t.Fatalf("err = %v, want ErrEmailNotVerified", err)
	}
	if len(f.patches) != 0 {
		t.Fatalf("refused approval still wrote %d patch(es)", len(f.patches))
	}
}

func TestReject_RefusesAnAccountThatIsNotAPendingSignup(t *testing.T) {
	u := pendingUser()
	u["is_active"] = true
	c, f := newFake(t, u)
	if _, err := c.Reject(context.Background(), 7); !errors.Is(err, ErrNotPending) {
		t.Fatalf("err = %v, want ErrNotPending", err)
	}
	if f.deletes != 0 {
		t.Fatalf("refused rejection still deleted %d account(s)", f.deletes)
	}
}

// The refusal must not carry the target account's address back to the client.
func TestNotPendingErrorCarriesNoAddress(t *testing.T) {
	if got := ErrNotPending.Error(); got == "" || contains(got, "@") {
		t.Fatalf("ErrNotPending leaks an address: %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
