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

type adminEnv struct {
	store  store.Store
	audit  *audit.Service
	router *chi.Mux
}

func newAdminEnv(t *testing.T) *adminEnv {
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
	mk := func(id, owner string) {
		if err := st.CreateTenant(ctx, &store.Tenant{ID: id, Slug: id, Name: "Tenant " + id, Plan: "free", Status: store.TenantActive, OwnerUserID: owner}); err != nil {
			t.Fatal(err)
		}
	}
	mk("acme", "u-acme")
	mk("beta", "")
	if err := st.CreateUser(ctx, "acme", &store.LocalUser{ID: "u-acme", Email: "owner@acme.example", Role: "owner", PasswordHash: "x", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for _, imei := range []string{"1", "2", "3", "4", "5"} {
		if err := st.CreateDevice(ctx, "acme", &store.Device{IMEI: "acme-" + imei, Label: imei}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateOrUpdateBridge(ctx, "beta", &store.Bridge{BridgeID: "beta-bridge", Label: "b"}); err != nil {
		t.Fatal(err)
	}
	env := &adminEnv{store: st, audit: audit.New(st)}
	h := NewTenantHandler(st)
	h.SetAudit(env.audit)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			u := &hubauth.User{ID: "u-admin", Email: "admin@platform.example", TenantID: store.DefaultTenantID, Roles: []string{"owner"}, PlatformAdmin: req.Header.Get("X-Test-User") != "customer", Session: true}
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), hubauth.UserContextKey, u)))
		})
	})
	r.Route("/api/admin/tenants", func(r chi.Router) {
		r.Use(hubauth.RequirePlatformAdmin())
		r.Get("/", h.AdminList)
		r.Get("/{id}", h.AdminGet)
		r.Put("/{id}", h.AdminUpdate)
		r.Get("/{id}/users", h.AdminListUsers)
		r.Get("/{id}/audit", h.AdminListAudit)
	})
	env.router = r
	return env
}

func (e *adminEnv) do(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

func TestAdminTenants_DirectoryCarriesOwnerCountsAndUsage(t *testing.T) {
	e := newAdminEnv(t)
	code, body := e.do(t, "GET", "/api/admin/tenants", "")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatal(err)
	}
	byID := map[string]map[string]any{}
	for _, r := range rows {
		byID[r["id"].(string)] = r
	}
	acme, beta := byID["acme"], byID["beta"]
	if acme["owner_email"] != "owner@acme.example" || acme["users"] != float64(1) || acme["devices"] != float64(5) || acme["used"] != float64(5) || acme["limit"] != float64(4) || acme["over_limit"] != true {
		t.Errorf("acme row: %v", acme)
	}
	if beta["bridges"] != float64(1) || beta["used"] != float64(1) || beta["over_limit"] != false {
		t.Errorf("beta row: %v", beta)
	}
	// Search narrows on the owner's address too.
	code, body = e.do(t, "GET", "/api/admin/tenants?q=owner%40acme", "")
	_ = json.Unmarshal(body, &rows)
	if code != 200 || len(rows) != 1 || rows[0]["id"] != "acme" {
		t.Errorf("q=owner@acme: %d %s", code, body)
	}
	// A customer never lists the directory.
	req := httptest.NewRequest("GET", "/api/admin/tenants", nil)
	req.Header.Set("X-Test-User", "customer")
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("customer listed tenants: %d", w.Code)
	}
}

func TestAdminTenants_DetailUsersAuditAndUpdateAreAuditedBothWays(t *testing.T) {
	e := newAdminEnv(t)
	if code, _ := e.do(t, "GET", "/api/admin/tenants/nope", ""); code != http.StatusNotFound {
		t.Errorf("unknown detail: %d", code)
	}
	code, body := e.do(t, "GET", "/api/admin/tenants/acme", "")
	var detail map[string]any
	_ = json.Unmarshal(body, &detail)
	if code != 200 || detail["owner_email"] != "owner@acme.example" || detail["support_access"] == nil {
		t.Fatalf("detail: %d %s", code, body)
	}
	code, body = e.do(t, "GET", "/api/admin/tenants/acme/users", "")
	if code != 200 || !strings.Contains(string(body), "owner@acme.example") || strings.Contains(string(body), "password") {
		t.Errorf("users: %d %s", code, body)
	}
	// Update: plan and a cap, with the change list on both chains.
	code, body = e.do(t, "PUT", "/api/admin/tenants/acme", `{"plan":"crew","ratelimit_daily_cap":250}`)
	if code != 200 {
		t.Fatalf("update: %d %s", code, body)
	}
	latest, _ := e.store.GetLatestAuditEntry(context.Background(), "acme")
	if latest == nil || latest.Action != "tenant_admin_updated" || !strings.Contains(latest.Detail, "plan=free>crew") || !strings.Contains(latest.Detail, "ratelimit_daily_cap=0>250") || latest.Actor != "admin@platform.example" {
		t.Errorf("tenant chain: %+v", latest)
	}
	platform, _ := e.store.GetLatestAuditEntry(context.Background(), store.DefaultTenantID)
	if platform == nil || platform.Action != "tenant_admin_updated" || !strings.HasPrefix(platform.Detail, "tenant=acme ") {
		t.Errorf("platform chain: %+v", platform)
	}
	// Nothing changed: no row.
	before, _ := e.store.ListAuditEntries(context.Background(), "acme", 100)
	if code, _ = e.do(t, "PUT", "/api/admin/tenants/acme", `{"plan":"crew"}`); code != 200 {
		t.Fatalf("no-op update: %d", code)
	}
	after, _ := e.store.ListAuditEntries(context.Background(), "acme", 100)
	if len(after) != len(before) {
		t.Errorf("a no-op update wrote %d audit rows", len(after)-len(before))
	}
	// The tenant's own audit, through the operator route.
	code, body = e.do(t, "GET", "/api/admin/tenants/acme/audit?limit=5", "")
	if code != 200 || !strings.Contains(string(body), "tenant_admin_updated") {
		t.Errorf("audit: %d %s", code, body)
	}
	// Isolation, victim = default: acme's entries never show under default's
	// route and default's never under acme's.
	code, body = e.do(t, "GET", "/api/admin/tenants/beta/audit", "")
	if code != 200 || strings.Contains(string(body), "plan=free>crew") {
		t.Errorf("beta's audit carries acme's row: %s", body)
	}
}

func TestAdminTenants_ReactivatingAClosedAccountClearsThePurgeDate(t *testing.T) {
	e := newAdminEnv(t)
	if err := e.store.SoftDeleteTenant(context.Background(), "beta", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// Closed accounts are hidden unless asked for.
	_, body := e.do(t, "GET", "/api/admin/tenants", "")
	if strings.Contains(string(body), `"id":"beta"`) {
		t.Errorf("closed tenant listed by default")
	}
	_, body = e.do(t, "GET", "/api/admin/tenants?include_deleted=1", "")
	if !strings.Contains(string(body), `"purge_at"`) {
		t.Errorf("closed tenant without purge_at: %s", body)
	}
	code, body := e.do(t, "PUT", "/api/admin/tenants/beta", `{"status":"active"}`)
	if code != 200 {
		t.Fatalf("reactivate: %d %s", code, body)
	}
	tn, _ := e.store.GetTenant(context.Background(), "beta")
	if tn.Status != store.TenantActive || tn.DeletedAt != nil {
		t.Errorf("after reactivation: status=%s deleted_at=%v", tn.Status, tn.DeletedAt)
	}
	if due, _ := e.store.ListTenantsDeletedBefore(context.Background(), time.Now().Add(365*24*time.Hour)); len(due) != 0 {
		t.Errorf("a reactivated tenant is still due for purge: %v", due)
	}
	latest, _ := e.store.GetLatestAuditEntry(context.Background(), "beta")
	if latest == nil || !strings.Contains(latest.Detail, "status=deleted>active") || !strings.Contains(latest.Detail, "deleted_at=") {
		t.Errorf("reactivation row: %+v", latest)
	}
}
