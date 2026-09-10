package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/authentik"
)

func signupRouter(h *SignupHandler) *chi.Mux {
	r := chi.NewRouter()
	r.Get("/api/admin/signups", h.List)
	r.Post("/api/admin/signups/{id}/approve", h.Approve)
	r.Post("/api/admin/signups/{id}/reject", h.Reject)
	return r
}

// With no identity provider configured the endpoints must say so plainly
// rather than 500: the panel hides itself on a 503, so a Hub deployed without
// an authentik token shows no half-working approval UI.
func TestSignups_UnconfiguredSaysSo(t *testing.T) {
	r := signupRouter(NewSignupHandler(nil, nil))
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/admin/signups"},
		{http.MethodPost, "/api/admin/signups/1/approve"},
		{http.MethodPost, "/api/admin/signups/1/reject"},
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", tc.method, tc.path, rec.Code)
		}
	}
}

// A role outside the three the Hub maps to must be refused before anything is
// changed at the identity provider.
func TestSignups_RejectsUnknownRole(t *testing.T) {
	// A non-nil client is needed to get past the readiness gate; it is never
	// reached, because the role is checked first.
	h := NewSignupHandler(fakeClient(t), nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/signups/1/approve",
		strings.NewReader(`{"role":"superuser"}`))
	req.Header.Set("Content-Type", "application/json")
	signupRouter(h).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if !strings.Contains(body["error"], "owner") {
		t.Errorf("error should name the valid roles, got %q", body["error"])
	}
}

// A malformed id is a client error, not a panic or a lookup.
func TestSignups_BadID(t *testing.T) {
	h := NewSignupHandler(fakeClient(t), nil)
	rec := httptest.NewRecorder()
	signupRouter(h).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/admin/signups/not-a-number/approve", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func fakeClient(t *testing.T) *authentik.Client {
	t.Helper()
	return authentik.New("http://127.0.0.1:1", "token-not-used")
}

// The signups routes are the sharpest thing a platform admin can do: approve
// activates an account on the shared identity provider and grants it a role.
// Every test above mounted them bare, so RequirePlatformAdmin existed only in
// main.go and deleting that line broke nothing in CI. This mounts them exactly
// as main.go does (MESHSAT-978).
func TestSignupRoutesRefuseAnyoneButAPlatformAdmin(t *testing.T) {
	mount := func(u *hubauth.User) *chi.Mux {
		h := NewSignupHandler(nil, nil) // unconfigured: a 503 means RBAC let it through
		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				ctx := req.Context()
				if u != nil {
					ctx = context.WithValue(ctx, hubauth.UserContextKey, u)
				}
				next.ServeHTTP(w, req.WithContext(ctx))
			})
		})
		r.Route("/api/admin/signups", func(r chi.Router) {
			r.Use(hubauth.RequirePlatformAdmin())
			r.Get("/", h.List)
			r.Post("/{id}/approve", h.Approve)
			r.Post("/{id}/reject", h.Reject)
		})
		return r
	}

	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/admin/signups/"},
		{http.MethodPost, "/api/admin/signups/1/approve"},
		{http.MethodPost, "/api/admin/signups/1/reject"},
	}

	for _, who := range []struct {
		name string
		user *hubauth.User
	}{
		{"nobody", nil},
		{"a tenant owner", &hubauth.User{ID: "u1", TenantID: "t1", Roles: []string{"owner"}}},
		{"an operator", &hubauth.User{ID: "u2", TenantID: "t1", Roles: []string{"operator"}}},
		{"a viewer", &hubauth.User{ID: "u3", TenantID: "t1", Roles: []string{"viewer"}}},
	} {
		r := mount(who.user)
		for _, rt := range routes {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(rt.method, rt.path, nil))
			if rec.Code == http.StatusServiceUnavailable {
				t.Errorf("%s reached %s %s -- RBAC is not on this route", who.name, rt.method, rt.path)
			}
			if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
				t.Errorf("%s on %s %s: got %d, want 401/403", who.name, rt.method, rt.path, rec.Code)
			}
		}
	}

	// And a platform admin does get through -- 503 here proves the handler was
	// reached, which is the point.
	r := mount(&hubauth.User{ID: "u4", TenantID: "t1", Roles: []string{"owner"}, PlatformAdmin: true})
	for _, rt := range routes {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(rt.method, rt.path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("platform admin on %s %s: got %d, want 503 (handler reached)", rt.method, rt.path, rec.Code)
		}
	}
}
