package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/quota"
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

// An operator-set plan is meant to be permanent, but nothing anywhere could
// write plan_expires_at, so a tenant that still carried a Ko-fi expiry lapsed
// straight back to free and the only fix was hand-written SQL (MESHSAT-989).
func TestAdminCanSetAndClearThePlanExpiry(t *testing.T) {
	s := newTenantStore(t)
	ctx := context.Background()
	when := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	tn, err := s.GetTenant(ctx, "t_acme")
	if err != nil {
		t.Fatal(err)
	}
	tn.PlanExpiresAt = &when
	if err := s.UpdateTenant(ctx, tn); err != nil {
		t.Fatal(err)
	}

	r := tenantRouter(t, s, &hubauth.User{ID: "u1", TenantID: "t_acme", Roles: []string{"owner"}, PlatformAdmin: true})

	// Absent leaves it alone.
	if rr := do(r, "PUT", "/api/admin/tenants/t_acme", `{"plan":"custom"}`); rr.Code != 200 {
		t.Fatalf("set plan: %d %s", rr.Code, rr.Body.String())
	}
	got, _ := s.GetTenant(ctx, "t_acme")
	if got.PlanExpiresAt == nil || !got.PlanExpiresAt.Equal(when) {
		t.Errorf("an absent field changed the expiry: %v", got.PlanExpiresAt)
	}

	// "" clears it -- the operator plan now never lapses.
	if rr := do(r, "PUT", "/api/admin/tenants/t_acme", `{"plan_expires_at":""}`); rr.Code != 200 {
		t.Fatalf("clear: %d %s", rr.Code, rr.Body.String())
	}
	got, _ = s.GetTenant(ctx, "t_acme")
	if got.PlanExpiresAt != nil {
		t.Errorf("expiry not cleared: %v", got.PlanExpiresAt)
	}

	// A date sets it.
	later := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second)
	body := `{"plan_expires_at":"` + later.Format(time.RFC3339) + `"}`
	if rr := do(r, "PUT", "/api/admin/tenants/t_acme", body); rr.Code != 200 {
		t.Fatalf("set date: %d %s", rr.Code, rr.Body.String())
	}
	got, _ = s.GetTenant(ctx, "t_acme")
	if got.PlanExpiresAt == nil || !got.PlanExpiresAt.Equal(later) {
		t.Errorf("expiry not set: %v want %v", got.PlanExpiresAt, later)
	}

	// Anything else is a 400, not a silent no-op.
	if rr := do(r, "PUT", "/api/admin/tenants/t_acme", `{"plan_expires_at":"next tuesday"}`); rr.Code != 400 {
		t.Errorf("garbage date: %d %s", rr.Code, rr.Body.String())
	}
}

// The usage endpoint tells the Settings page which billing surface to draw.
//
// It used to mint a claim code here, because the old provider had no way to
// carry a tenant id through checkout and the customer had to quote one in a
// message box. A Checkout session carries metadata, so there is nothing to
// copy and nothing to forget (MESHSAT-1023).
func TestUsageReportsWhichBillingSurfaceToDraw(t *testing.T) {
	s := newTenantStore(t)
	q := quota.New(s, func(ctx context.Context, id string) (string, error) {
		tn, err := s.GetTenant(ctx, id)
		if err != nil || tn == nil {
			return plans.Free, err
		}
		return tn.Plan, nil
	})
	h := NewTenantUsageHandler(q, s)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := context.WithValue(req.Context(), hubauth.UserContextKey,
				&hubauth.User{ID: "u1", TenantID: "t_acme", Roles: []string{"owner"}})
			ctx = context.WithValue(ctx, hubauth.TenantContextKey, "t_acme")
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Get("/api/tenant/usage", h.Usage)

	read := func() map[string]any {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/api/tenant/usage", nil))
		if w.Code != 200 {
			t.Fatalf("usage: %d %s", w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	// The claim code is gone from the wire shape entirely: leaving it would
	// have the UI keep telling customers to quote something that no longer
	// matches anything.
	if _, ok := read()["claim_code"]; ok {
		t.Error("the response still carries a claim code")
	}

	SetStripeReady(true)
	defer SetStripeReady(false)
	b, _ := read()["billing"].(map[string]any)
	if b == nil {
		t.Fatal("no billing state in the usage response")
	}
	if b["provider"] != "stripe" {
		t.Errorf("provider = %v, want stripe", b["provider"])
	}
	// Nothing has been paid for this tenant yet, so there is nothing to manage
	// and the portal button must not be offered.
	if b["manageable"] != false {
		t.Errorf("manageable = %v for a tenant with no Stripe customer", b["manageable"])
	}

	tn, err := s.GetTenant(t.Context(), "t_acme")
	if err != nil {
		t.Fatal(err)
	}
	tn.StripeCustomerID = "cus_1"
	if err := s.UpdateTenant(t.Context(), tn); err != nil {
		t.Fatal(err)
	}
	b, _ = read()["billing"].(map[string]any)
	if b["manageable"] != true {
		t.Errorf("manageable = %v once there is a Stripe customer", b["manageable"])
	}
}
