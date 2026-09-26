package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// MESHSAT-1366. A PERSON signed in as platform admin may act on another
// tenant only after that tenant's owner granted support access and the PIN
// was presented. The break-glass token and platform API keys keep the
// header: they are not people browsing, and they are audited per use.
//
// The victim is the default tenant's neighbour "customer": the property is
// about reaching ANY tenant that is not one's own.
func TestTenantMiddleware_ASessionAdminNeedsTheCustomersConsent(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Resolved-Tenant", TenantIDFromContext(r.Context()))
		w.WriteHeader(200)
	})
	mw := TenantMiddleware(false)
	call := func(u *User, header string) (int, string) {
		req := httptest.NewRequest("GET", "/api/devices", nil)
		req.Header.Set("X-Tenant-ID", header)
		req = req.WithContext(context.WithValue(req.Context(), UserContextKey, u))
		w := httptest.NewRecorder()
		mw(inner).ServeHTTP(w, req)
		return w.Code, w.Header().Get("X-Resolved-Tenant")
	}
	session := &User{ID: "admin", Email: "admin@example.org", TenantID: "default", PlatformAdmin: true, Session: true}
	breakGlass := &User{ID: "token-user", Roles: []string{"admin"}, PlatformAdmin: true}
	platformKey := &User{ID: "key-1", TenantID: "default", Roles: []string{"owner"}, PlatformAdmin: true}

	// No lookup wired at all: a session admin is refused, the others pass.
	SetSupportGrantLookup(nil)
	if code, _ := call(session, "customer"); code != http.StatusForbidden {
		t.Fatalf("session admin without any grant lookup: %d, want 403", code)
	}
	for _, u := range []*User{breakGlass, platformKey} {
		if code, got := call(u, "customer"); code != 200 || got != "customer" {
			t.Fatalf("%s: %d %q, want 200 customer", u.ID, code, got)
		}
	}

	// A lookup that says "not opened": refused; own tenant and default never ask.
	asked := 0
	SetSupportGrantLookup(func(_ context.Context, tenantID string) (bool, error) {
		asked++
		return false, nil
	})
	if code, _ := call(session, "customer"); code != http.StatusForbidden {
		t.Fatalf("session admin, grant not opened: %d, want 403", code)
	}
	for _, own := range []string{"default"} {
		if code, got := call(session, own); code != 200 || got != own {
			t.Fatalf("own tenant %q: %d %q", own, code, got)
		}
	}
	if asked != 1 {
		t.Fatalf("lookup asked %d times, want once (own tenant never asks)", asked)
	}

	// Opened: honoured.
	SetSupportGrantLookup(func(_ context.Context, tenantID string) (bool, error) { return tenantID == "customer", nil })
	if code, got := call(session, "customer"); code != 200 || got != "customer" {
		t.Fatalf("opened grant: %d %q, want 200 customer", code, got)
	}
	if code, _ := call(session, "other"); code != http.StatusForbidden {
		t.Fatalf("a grant for one tenant opened another: %d", code)
	}

	// A lookup error fails CLOSED.
	SetSupportGrantLookup(func(context.Context, string) (bool, error) { return true, errors.New("db down") })
	if code, _ := call(session, "customer"); code != http.StatusForbidden {
		t.Fatalf("lookup error opened the tenant: %d", code)
	}
	SetSupportGrantLookup(nil)
}
