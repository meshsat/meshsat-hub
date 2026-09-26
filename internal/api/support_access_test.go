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

	"github.com/meshsat/meshsat-hub/internal/audit"
	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// supportEnv mounts the customer's support-access routes and the operator's
// view-as routes the way main.go does, over a real sqlite store and audit
// chain, so a test exercises the same middleware stack production runs.
type supportEnv struct {
	store   store.Store
	audit   *audit.Service
	router  *chi.Mux
	forgets []string
}

func newSupportEnv(t *testing.T) *supportEnv {
	t.Helper()
	st, err := sqlite.New(":memory:", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, id := range []string{"cust", "other"} {
		if err := st.CreateTenant(ctx, &store.Tenant{ID: id, Slug: id, Name: "Customer " + id, Plan: "free", Status: store.TenantActive}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateTenant(ctx, &store.Tenant{ID: "closed", Slug: "closed", Name: "Closed", Plan: "free", Status: store.TenantActive}); err != nil {
		t.Fatal(err)
	}
	if err := st.SoftDeleteTenant(ctx, "closed", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	env := &supportEnv{store: st, audit: audit.New(st)}
	forget := func(id string) { env.forgets = append(env.forgets, id) }
	sa := NewSupportAccessHandler(st, env.audit, 15, 4320, forget)
	va := NewViewAsHandler(st, env.audit, forget)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			// The test names the caller in a header; production's auth
			// middleware does the same job from a session.
			ctx := req.Context()
			switch req.Header.Get("X-Test-User") {
			case "owner":
				ctx = context.WithValue(ctx, hubauth.UserContextKey, &hubauth.User{ID: "u-owner", Email: "owner@cust.example", TenantID: "cust", Roles: []string{"owner"}, Session: true})
				ctx = context.WithValue(ctx, hubauth.TenantContextKey, "cust")
			case "platform-owner":
				ctx = context.WithValue(ctx, hubauth.UserContextKey, &hubauth.User{ID: "u-plat", Email: "admin@platform.example", TenantID: store.DefaultTenantID, Roles: []string{"owner"}, PlatformAdmin: true, Session: true})
				ctx = context.WithValue(ctx, hubauth.TenantContextKey, store.DefaultTenantID)
			case "viewer":
				ctx = context.WithValue(ctx, hubauth.UserContextKey, &hubauth.User{ID: "u-view", Email: "viewer@cust.example", TenantID: "cust", Roles: []string{"viewer"}, Session: true})
				ctx = context.WithValue(ctx, hubauth.TenantContextKey, "cust")
			case "admin":
				ctx = context.WithValue(ctx, hubauth.UserContextKey, &hubauth.User{ID: "u-admin", Email: "admin@platform.example", TenantID: store.DefaultTenantID, Roles: []string{"owner"}, PlatformAdmin: true, Session: true})
				ctx = context.WithValue(ctx, hubauth.TenantContextKey, store.DefaultTenantID)
			}
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Route("/api/tenant", func(r chi.Router) {
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Get("/support-access", sa.Get)
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Post("/support-access", sa.Create)
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Delete("/support-access", sa.Revoke)
	})
	r.Route("/api/admin/tenants", func(r chi.Router) {
		r.Use(hubauth.RequirePlatformAdmin())
		r.Post("/{id}/view-as", va.Start)
		r.Delete("/{id}/view-as", va.End)
	})
	env.router = r
	return env
}

func (e *supportEnv) do(t *testing.T, as, method, path, body string) (int, map[string]any) {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Test-User", as)
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func (e *supportEnv) actions(t *testing.T, tenantID string) []string {
	t.Helper()
	entries, err := e.store.ListAuditEntries(context.Background(), tenantID, 50)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, x := range entries {
		out = append(out, x.Action)
	}
	return out
}

func has(list []string, want string) bool {
	for _, x := range list {
		if x == want {
			return true
		}
	}
	return false
}

func TestSupportAccess_OwnerGrantsWithinBoundsAndItIsAudited(t *testing.T) {
	e := newSupportEnv(t)
	code, body := e.do(t, "owner", "GET", "/api/tenant/support-access", "")
	if code != 200 || body["active"] != false || body["min_minutes"] != float64(15) || body["max_minutes"] != float64(4320) {
		t.Fatalf("initial status: %d %v", code, body)
	}
	for _, bad := range []string{
		`{"pin":"short","duration_minutes":60}`,
		`{"pin":"` + strings.Repeat("x", 129) + `","duration_minutes":60}`,
		`{"pin":"long-enough-pin","duration_minutes":5}`,
		`{"pin":"long-enough-pin","duration_minutes":99999}`,
	} {
		if code, _ := e.do(t, "owner", "POST", "/api/tenant/support-access", bad); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", bad, code)
		}
	}
	code, body = e.do(t, "owner", "POST", "/api/tenant/support-access", `{"pin":"correct horse battery","duration_minutes":30}`)
	if code != http.StatusCreated || body["active"] != true || body["created_by_email"] != "owner@cust.example" {
		t.Fatalf("grant: %d %v", code, body)
	}
	exp, _ := time.Parse(time.RFC3339, body["expires_at"].(string))
	if d := time.Until(exp); d < 28*time.Minute || d > 31*time.Minute {
		t.Errorf("expiry %v from now, want about 30 min", d)
	}
	acts := e.actions(t, "cust")
	if !has(acts, "support_access_granted") {
		t.Errorf("no support_access_granted on the tenant chain: %v", acts)
	}
	entries, _ := e.store.ListAuditEntries(context.Background(), "cust", 5)
	for _, x := range entries {
		if strings.Contains(x.Detail, "correct horse") {
			t.Fatalf("the PIN is in the audit detail: %q", x.Detail)
		}
	}
	// A viewer cannot grant; the platform tenant cannot grant to itself.
	if code, _ := e.do(t, "viewer", "POST", "/api/tenant/support-access", `{"pin":"correct horse battery","duration_minutes":30}`); code != http.StatusForbidden {
		t.Errorf("viewer granted: %d", code)
	}
	if code, _ := e.do(t, "platform-owner", "POST", "/api/tenant/support-access", `{"pin":"correct horse battery","duration_minutes":30}`); code != http.StatusBadRequest {
		t.Errorf("platform tenant granted to itself: %d", code)
	}
	// Revoke.
	code, body = e.do(t, "owner", "DELETE", "/api/tenant/support-access", "")
	if code != 200 || body["active"] != false {
		t.Fatalf("revoke: %d %v", code, body)
	}
	if !has(e.actions(t, "cust"), "support_access_revoked") {
		t.Errorf("no support_access_revoked on the tenant chain")
	}
	if len(e.forgets) < 2 {
		t.Errorf("the grant cache was not told about the grant and the revoke: %v", e.forgets)
	}
}

func TestViewAs_NeedsTheCustomersPINAndIsAuditedBothWays(t *testing.T) {
	e := newSupportEnv(t)
	// Nobody granted: refused with the code the console branches on.
	code, body := e.do(t, "admin", "POST", "/api/admin/tenants/cust/view-as", `{"pin":"whatever-pin-here"}`)
	if code != http.StatusForbidden || body["code"] != "support_access_required" {
		t.Fatalf("no grant: %d %v", code, body)
	}
	if code, _ := e.do(t, "admin", "POST", "/api/admin/tenants/default/view-as", `{"pin":"whatever-pin-here"}`); code != http.StatusBadRequest {
		t.Errorf("own tenant: %d, want 400", code)
	}
	if code, _ := e.do(t, "admin", "POST", "/api/admin/tenants/nope/view-as", `{"pin":"whatever-pin-here"}`); code != http.StatusNotFound {
		t.Errorf("unknown tenant: %d, want 404", code)
	}
	if code, _ := e.do(t, "owner", "POST", "/api/admin/tenants/cust/view-as", `{"pin":"whatever-pin-here"}`); code != http.StatusForbidden {
		t.Errorf("a tenant owner reached the operator route: %d", code)
	}
	// The customer grants.
	if code, _ := e.do(t, "owner", "POST", "/api/tenant/support-access", `{"pin":"correct horse battery","duration_minutes":60}`); code != http.StatusCreated {
		t.Fatalf("grant: %d", code)
	}
	// Wrong PINs count down; the fifth locks the grant.
	for i := 1; i <= 4; i++ {
		code, body = e.do(t, "admin", "POST", "/api/admin/tenants/cust/view-as", `{"pin":"wrong-pin-attempt"}`)
		if code != http.StatusUnauthorized || body["code"] != "wrong_pin" || body["attempts_left"] != float64(5-i) {
			t.Fatalf("wrong pin %d: %d %v", i, code, body)
		}
	}
	code, body = e.do(t, "admin", "POST", "/api/admin/tenants/cust/view-as", `{"pin":"wrong-pin-attempt"}`)
	if code != http.StatusForbidden || body["code"] != "support_access_locked" {
		t.Fatalf("fifth wrong pin: %d %v", code, body)
	}
	if !has(e.actions(t, "cust"), "support_access_locked") || !has(e.actions(t, store.DefaultTenantID), "support_access_locked") {
		t.Errorf("lockout not on both chains: cust=%v default=%v", e.actions(t, "cust"), e.actions(t, store.DefaultTenantID))
	}
	// Even the right PIN is refused now: the customer has to grant again.
	if code, body = e.do(t, "admin", "POST", "/api/admin/tenants/cust/view-as", `{"pin":"correct horse battery"}`); code != http.StatusForbidden || body["code"] != "support_access_required" {
		t.Fatalf("right pin after lockout: %d %v", code, body)
	}
	if code, _ := e.do(t, "owner", "POST", "/api/tenant/support-access", `{"pin":"second try pin","duration_minutes":60}`); code != http.StatusCreated {
		t.Fatalf("re-grant: %d", code)
	}
	code, body = e.do(t, "admin", "POST", "/api/admin/tenants/cust/view-as", `{"pin":"second try pin"}`)
	if code != 200 || body["tenant_id"] != "cust" || body["slug"] != "cust" || body["expires_at"] == nil {
		t.Fatalf("open: %d %v", code, body)
	}
	g, err := e.store.GetActiveSupportGrant(context.Background(), "cust")
	if err != nil || !g.Opened() || g.UsedByEmail != "admin@platform.example" {
		t.Fatalf("grant after open: %+v %v", g, err)
	}
	cust, def := e.actions(t, "cust"), e.actions(t, store.DefaultTenantID)
	if !has(cust, "tenant_view_started") || !has(def, "tenant_view_started") {
		t.Errorf("view start not on both chains: cust=%v default=%v", cust, def)
	}
	entries, _ := e.store.ListAuditEntries(context.Background(), store.DefaultTenantID, 3)
	if !strings.Contains(entries[0].Detail, "tenant=cust") || entries[0].Actor != "admin@platform.example" {
		t.Errorf("platform row = %+v, want tenant=cust by the admin", entries[0])
	}
	// Ending is audited both ways too.
	if code, _ = e.do(t, "admin", "DELETE", "/api/admin/tenants/cust/view-as", ""); code != 200 {
		t.Fatalf("end: %d", code)
	}
	if !has(e.actions(t, "cust"), "tenant_view_ended") || !has(e.actions(t, store.DefaultTenantID), "tenant_view_ended") {
		t.Errorf("view end not on both chains")
	}
	for _, tid := range []string{"cust", store.DefaultTenantID} {
		if _, broken, err := e.audit.VerifyChain(context.Background(), tid); err != nil || broken != nil {
			t.Errorf("chain %s broken: %v %v", tid, broken, err)
		}
	}
	// The cache learns of the open and the lock.
	if len(e.forgets) < 3 {
		t.Errorf("cache forgets: %v", e.forgets)
	}
	// A closed tenant cannot be opened.
	if code, body = e.do(t, "admin", "POST", "/api/admin/tenants/closed/view-as", `{"pin":"x"}`); code != http.StatusConflict || body["code"] != "not_active" {
		t.Errorf("closed tenant: %d %v", code, body)
	}
}

// The grant cache answers "opened" only for an opened, unexpired grant, and
// forgets on demand.
func TestSupportGrantCache(t *testing.T) {
	st, err := sqlite.New(":memory:", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.CreateTenant(ctx, &store.Tenant{ID: "c1", Slug: "c1", Name: "c1", Plan: "free", Status: store.TenantActive}); err != nil {
		t.Fatal(err)
	}
	c := NewSupportGrantCache(st, time.Hour)
	if ok, err := c.Opened(ctx, "c1"); ok || err != nil {
		t.Fatalf("no grant: %v %v", ok, err)
	}
	g := &store.SupportGrant{PINHash: "h", ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.CreateSupportGrant(ctx, "c1", g); err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.Opened(ctx, "c1"); ok {
		t.Fatal("cached negative answer changed without Forget")
	}
	c.Forget("c1")
	if ok, _ := c.Opened(ctx, "c1"); ok {
		t.Fatal("a grant nobody opened counts as opened")
	}
	if err := st.MarkSupportGrantUsed(ctx, "c1", g.ID, "op", time.Now()); err != nil {
		t.Fatal(err)
	}
	c.Forget("c1")
	if ok, _ := c.Opened(ctx, "c1"); !ok {
		t.Fatal("an opened grant is not reported")
	}
}
