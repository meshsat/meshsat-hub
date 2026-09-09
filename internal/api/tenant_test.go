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
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// tenantRouter mounts the tenant routes exactly as main.go does, with the
// given user injected and the tenant resolved by the real middleware.
func tenantRouter(t *testing.T, s store.Store, user *hubauth.User) *chi.Mux {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := req.Context()
			if user != nil {
				ctx = context.WithValue(ctx, hubauth.UserContextKey, user)
			}
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Use(hubauth.TenantMiddleware(true))
	h := NewTenantHandler(s)
	r.Route("/api/tenant", func(r chi.Router) {
		r.With(hubauth.RequireRole(hubauth.RoleViewer)).Get("/", h.Get)
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Put("/", h.Update)
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Get("/invites", h.ListInvites)
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Post("/invites", h.CreateInvite)
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Delete("/invites/{id}", h.DeleteInvite)
	})
	r.Route("/api/admin/tenants", func(r chi.Router) {
		r.Use(hubauth.RequirePlatformAdmin())
		r.Get("/", h.AdminList)
		r.Put("/{id}", h.AdminUpdate)
	})
	return r
}

func newTenantStore(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlite.New(":memory:", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.CreateTenant(context.Background(), &store.Tenant{ID: "t_acme", Slug: "acme", Name: "ACME", Plan: "beta", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	return s
}

func do(r http.Handler, method, path, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func TestTenant_OwnerFlow(t *testing.T) {
	s := newTenantStore(t)
	owner := &hubauth.User{ID: "u1", Email: "o@acme.org", Roles: []string{"owner"}, TenantID: "t_acme"}
	r := tenantRouter(t, s, owner)

	rr := do(r, "GET", "/api/tenant", "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"slug":"acme"`) {
		t.Fatalf("get: %d %s", rr.Code, rr.Body.String())
	}
	rr = do(r, "PUT", "/api/tenant", `{"name":"ACME Field Ops"}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "ACME Field Ops") {
		t.Fatalf("rename: %d %s", rr.Code, rr.Body.String())
	}
	if rr = do(r, "PUT", "/api/tenant", `{"name":""}`); rr.Code != 400 {
		t.Fatalf("empty name accepted: %d", rr.Code)
	}
	if rr = do(r, "PUT", "/api/tenant", `{"name":"x","extra":1}`); rr.Code != 400 {
		t.Fatalf("unknown field accepted: %d", rr.Code)
	}

	// Invite: validation, create, duplicate, list, revoke.
	if rr = do(r, "POST", "/api/tenant/invites", `{"email":"nope","role":"operator"}`); rr.Code != 400 {
		t.Fatalf("bad email accepted: %d", rr.Code)
	}
	if rr = do(r, "POST", "/api/tenant/invites", `{"email":"a@b.org","role":"root"}`); rr.Code != 400 {
		t.Fatalf("bad role accepted: %d", rr.Code)
	}
	rr = do(r, "POST", "/api/tenant/invites", `{"email":"Carol@Example.org","role":"operator"}`)
	if rr.Code != 201 {
		t.Fatalf("invite: %d %s", rr.Code, rr.Body.String())
	}
	var inv inviteResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &inv)
	if inv.Email != "carol@example.org" || inv.Role != "operator" || inv.AcceptedAt != "" || inv.ID == "" {
		t.Fatalf("invite body: %+v", inv)
	}
	if rr = do(r, "POST", "/api/tenant/invites", `{"email":"carol@example.org","role":"viewer"}`); rr.Code != 409 {
		t.Fatalf("duplicate invite: %d", rr.Code)
	}
	rr = do(r, "GET", "/api/tenant/invites", "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), inv.ID) {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	// The JIT lookup sees it as pending.
	if p, err := s.GetPendingInviteByEmail(context.Background(), "carol@example.org"); err != nil || p == nil || p.TenantID != "t_acme" {
		t.Fatalf("pending lookup: %+v %v", p, err)
	}
	if rr = do(r, "DELETE", "/api/tenant/invites/"+inv.ID, ""); rr.Code != 204 {
		t.Fatalf("revoke: %d", rr.Code)
	}
	if p, _ := s.GetPendingInviteByEmail(context.Background(), "carol@example.org"); p != nil {
		t.Fatal("invite still pending after revoke")
	}
	// Existing member cannot be invited again.
	if err := s.CreateUser(context.Background(), "t_acme", &store.LocalUser{ID: "u2", Email: "dave@acme.org", Name: "Dave", Role: "viewer", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if rr = do(r, "POST", "/api/tenant/invites", `{"email":"dave@acme.org"}`); rr.Code != 409 {
		t.Fatalf("member invited: %d", rr.Code)
	}
}

func TestTenant_ViewerAndCrossTenant(t *testing.T) {
	s := newTenantStore(t)
	viewer := &hubauth.User{ID: "v1", Roles: []string{"viewer"}, TenantID: "t_acme"}
	r := tenantRouter(t, s, viewer)
	if rr := do(r, "GET", "/api/tenant", ""); rr.Code != 200 {
		t.Fatalf("viewer get: %d", rr.Code)
	}
	if rr := do(r, "PUT", "/api/tenant", `{"name":"x"}`); rr.Code != 403 {
		t.Fatalf("viewer rename: %d", rr.Code)
	}
	if rr := do(r, "GET", "/api/tenant/invites", ""); rr.Code != 403 {
		t.Fatalf("viewer invites: %d", rr.Code)
	}
	if rr := do(r, "GET", "/api/admin/tenants", ""); rr.Code != 403 {
		t.Fatalf("viewer admin list: %d", rr.Code)
	}
	// X-Tenant-ID from a non-admin is ignored: still their own tenant.
	if rr := do(r, "GET", "/api/tenant", "", "X-Tenant-ID", "default"); rr.Code != 200 || !strings.Contains(rr.Body.String(), `"id":"t_acme"`) {
		t.Fatalf("header honoured for member: %d %s", rr.Code, rr.Body.String())
	}
	// No user at all: 403 (enforce) rather than the default tenant.
	anon := tenantRouter(t, s, nil)
	if rr := do(anon, "GET", "/api/tenant", ""); rr.Code != 403 {
		t.Fatalf("anonymous: %d", rr.Code)
	}
}

func TestTenant_PlatformAdmin(t *testing.T) {
	s := newTenantStore(t)
	admin := &hubauth.User{ID: "adm", Roles: []string{"owner"}, TenantID: "default", PlatformAdmin: true}
	r := tenantRouter(t, s, admin)
	rr := do(r, "GET", "/api/admin/tenants", "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"acme"`) || !strings.Contains(rr.Body.String(), `"default"`) {
		t.Fatalf("admin list: %d %s", rr.Code, rr.Body.String())
	}
	// Acts on another tenant through X-Tenant-ID.
	rr = do(r, "GET", "/api/tenant", "", "X-Tenant-ID", "t_acme")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"id":"t_acme"`) {
		t.Fatalf("admin header: %d %s", rr.Code, rr.Body.String())
	}
	rr = do(r, "PUT", "/api/admin/tenants/t_acme", `{"status":"suspended","plan":"crew"}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"status":"suspended"`) || !strings.Contains(rr.Body.String(), `"plan":"crew"`) {
		t.Fatalf("admin update: %d %s", rr.Code, rr.Body.String())
	}
	// A plan name now decides a device ceiling, so it has to be a real tier.
	// This used to accept any string under 32 bytes, which would have silently
	// dropped the tenant to the free limits (MESHSAT-989).
	if rr = do(r, "PUT", "/api/admin/tenants/t_acme", `{"plan":"pro"}`); rr.Code != 400 {
		t.Fatalf("unknown plan accepted: %d %s", rr.Code, rr.Body.String())
	}
	if rr = do(r, "PUT", "/api/admin/tenants/default", `{"status":"suspended"}`); rr.Code != 400 {
		t.Fatalf("default suspended: %d", rr.Code)
	}
	if rr = do(r, "PUT", "/api/admin/tenants/nope", `{"name":"x"}`); rr.Code != 404 {
		t.Fatalf("missing tenant: %d", rr.Code)
	}
	if rr = do(r, "PUT", "/api/admin/tenants/t_acme", `{"status":"deleted"}`); rr.Code != 400 {
		t.Fatalf("bad status: %d", rr.Code)
	}
}

func TestValidEmail(t *testing.T) {
	for in, want := range map[string]bool{"a@b.org": true, "A.b+c@sub.example.co": true, "nope": false, "@b.org": false, "a@b": false, "a b@c.org": false, "a@b.org\n": false} {
		if got := validEmail(in); got != want {
			t.Errorf("validEmail(%q)=%v", in, got)
		}
	}
}
