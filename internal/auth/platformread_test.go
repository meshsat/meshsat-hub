package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// MESHSAT-1527. RequirePlatformRead is the gate of the read-only platform key:
// a platform READER passes on GET and HEAD and on nothing else, a platform
// admin passes on everything, and every other caller is refused the way
// RequirePlatformAdmin refuses. The reader must also never pass
// RequirePlatformAdmin itself, which is the whole reason the axis exists: the
// admin flag cannot be narrowed by a key's role.
//
// ⚠ Do not weaken these. If a reader row ever needs to say 200 on a write to
// pass, the change is wrong.

func reqAs(method string, u *User) *http.Request {
	req := httptest.NewRequest(method, "/api/admin/tenants", nil)
	if u != nil {
		req = req.WithContext(context.WithValue(req.Context(), UserContextKey, u))
	}
	return req
}

func TestRequirePlatformRead(t *testing.T) {
	reader := &User{ID: "apikey:key-reader", Roles: []string{RoleViewer}, TenantID: "default", PlatformReader: true}
	admin := &User{ID: "apikey:key-admin", Roles: []string{RoleOwner}, TenantID: "default", PlatformAdmin: true}
	ownerKey := &User{ID: "apikey:key-owner", Roles: []string{RoleOwner}, TenantID: "t-customer"}
	sessionOwner := &User{ID: "u-1", Roles: []string{RoleOwner}, TenantID: "t-customer", Session: true}

	reads := []string{http.MethodGet, http.MethodHead}
	writes := []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

	type row struct {
		name   string
		user   *User
		method string
		want   int
	}
	var rows []row
	for _, m := range reads {
		rows = append(rows,
			row{"reader", reader, m, http.StatusOK},
			row{"platform admin", admin, m, http.StatusOK},
			row{"tenant owner key", ownerKey, m, http.StatusForbidden},
			row{"signed-in tenant owner", sessionOwner, m, http.StatusForbidden},
			row{"nobody", nil, m, http.StatusForbidden},
		)
	}
	for _, m := range writes {
		rows = append(rows,
			row{"reader", reader, m, http.StatusForbidden},
			row{"platform admin", admin, m, http.StatusOK},
			row{"tenant owner key", ownerKey, m, http.StatusForbidden},
			row{"nobody", nil, m, http.StatusForbidden},
		)
	}

	for _, tc := range rows {
		t.Run(tc.name+" "+tc.method, func(t *testing.T) {
			reached := false
			h := RequirePlatformRead()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, reqAs(tc.method, tc.user))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if reached != (tc.want == http.StatusOK) {
				t.Fatalf("handler reached = %v with status %d", reached, rec.Code)
			}
		})
	}
}

// The reader is not an admin, on any method: every platform-admin route stays
// shut to it, reads included (tenant audit, payments, receipts, VAT).
func TestAPlatformReaderNeverPassesRequirePlatformAdmin(t *testing.T) {
	reader := &User{ID: "apikey:key-reader", Roles: []string{RoleViewer}, TenantID: "default", PlatformReader: true}
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete} {
		h := RequirePlatformAdmin()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("a platform reader reached a platform-admin handler on %s", m)
		}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, reqAs(m, reader))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", m, rec.Code)
		}
	}
}

// A refusal is counted under its own requirement label, which exists before
// the first refusal (see TestSecurityCountersExistBeforeFirstUse).
func TestPlatformReadDenialIsCounted(t *testing.T) {
	lbl := map[string]string{"requirement": "platform_read", "channel": "internet"}
	before := counterValue(t, "meshsat_hub_authz_denials_total", lbl)
	if before < 0 {
		t.Fatal("meshsat_hub_authz_denials_total{requirement=\"platform_read\"} is not materialised at startup")
	}
	reader := &User{ID: "apikey:key-reader", Roles: []string{RoleViewer}, PlatformReader: true}
	rec := httptest.NewRecorder()
	RequirePlatformRead()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(rec, reqAs(http.MethodPut, reader))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if after := counterValue(t, "meshsat_hub_authz_denials_total", lbl); after != before+1 {
		t.Errorf("platform_read denials went %v -> %v, want +1", before, after)
	}
}

// X-Tenant-ID is the platform ADMIN's switch. A reader key sending it stays in
// its own tenant: reading the directory is not opening a workspace.
func TestAPlatformReaderCannotSwitchTenant(t *testing.T) {
	reader := &User{ID: "apikey:key-reader", Roles: []string{RoleViewer}, TenantID: "default", PlatformReader: true}
	var got string
	h := TenantMiddleware(true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = TenantIDFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/devices", nil)
	req.Header.Set("X-Tenant-ID", "t-customer")
	req = req.WithContext(context.WithValue(req.Context(), UserContextKey, reader))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "default" {
		t.Fatalf("a platform reader's X-Tenant-ID moved it to %q; want its own tenant", got)
	}
}
