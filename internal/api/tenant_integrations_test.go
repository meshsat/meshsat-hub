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
	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// credStore keeps credentials in memory so the handler round-trips through
// the real integrations service (encryption included).
type credStore struct {
	*mockStore
	rows map[string]*store.Credential
}

func (s *credStore) CreateCredential(_ context.Context, tenantID string, c *store.Credential) error {
	c.TenantID = tenantID
	cp := *c
	s.rows[c.ID] = &cp
	return nil
}
func (s *credStore) UpdateCredential(_ context.Context, tenantID string, c *store.Credential) error {
	cp := *c
	cp.TenantID = tenantID
	s.rows[c.ID] = &cp
	return nil
}
func (s *credStore) DeleteCredential(_ context.Context, _ string, id string) error {
	delete(s.rows, id)
	return nil
}
func (s *credStore) ListCredentials(_ context.Context, tenantID string) ([]store.Credential, error) {
	var out []store.Credential
	for _, c := range s.rows {
		if c.TenantID == tenantID {
			out = append(out, *c)
		}
	}
	return out, nil
}

func integrationsRouter(t *testing.T, tenantID, role string) (http.Handler, *integrations.Service) {
	t.Helper()
	key := make([]byte, 32)
	svc := integrations.New(&credStore{mockStore: &mockStore{}, rows: map[string]*store.Credential{}}, key)
	h := NewTenantIntegrationsHandler(svc, nil)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := context.WithValue(req.Context(), hubauth.UserContextKey, &hubauth.User{ID: "u1", Email: "owner@example.org", Roles: []string{role}, TenantID: tenantID})
			ctx = context.WithValue(ctx, hubauth.TenantContextKey, tenantID)
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Route("/api/tenant", func(r chi.Router) {
		r.With(hubauth.RequireRole(hubauth.RoleViewer)).Get("/integrations", h.List)
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Put("/integrations/{provider}", h.Put)
		r.With(hubauth.RequireRole(hubauth.RoleOwner)).Delete("/integrations/{provider}", h.Delete)
	})
	return r, svc
}

func doReq(h http.Handler, method, url, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestTenantIntegrations_PutListMaskDelete(t *testing.T) {
	r, svc := integrationsRouter(t, "t1", hubauth.RoleOwner)

	if rr := doReq(r, http.MethodPut, "/api/tenant/integrations/nope", `{"values":{}}`); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown provider: %d", rr.Code)
	}
	if rr := doReq(r, http.MethodPut, "/api/tenant/integrations/twilio", `{"values":{"account_sid":"AC1"}}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("missing required: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doReq(r, http.MethodPut, "/api/tenant/integrations/cloudloop", `{"values":{"api_key":"secret-key-123456","bogus":"x"}}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", rr.Code)
	}
	rr := doReq(r, http.MethodPut, "/api/tenant/integrations/cloudloop", `{"values":{"api_key":"secret-key-123456"}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rr.Code, rr.Body.String())
	}
	var put struct {
		Account struct {
			Configured  bool              `json:"configured"`
			Platform    bool              `json:"platform"`
			Values      map[string]string `json:"values"`
			WebhookPath string            `json:"webhook_path"`
		} `json:"account"`
		Reveal map[string]string `json:"reveal"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &put)
	if !put.Account.Configured || put.Account.Platform || put.Account.Values["api_key"] != "••••3456" {
		t.Errorf("account view: %+v", put.Account)
	}
	if put.Reveal["webhook_token"] == "" || len(put.Reveal["webhook_token"]) < 20 {
		t.Errorf("generated webhook token not revealed: %v", put.Reveal)
	}
	// The tenant's own endpoint, with its secret as the last path segment
	// (MESHSAT-975). It is returned in full: a masked URL cannot be pasted
	// into a provider console, which is the only reason this field exists.
	wantPrefix := "/api/webhook/cloudloop/"
	if !strings.HasPrefix(put.Account.WebhookPath, wantPrefix) {
		t.Errorf("webhook path = %q, want prefix %q", put.Account.WebhookPath, wantPrefix)
	}
	if secret := strings.TrimPrefix(put.Account.WebhookPath, wantPrefix); len(secret) < 16 || strings.Contains(secret, "•") {
		t.Errorf("webhook path carries no usable secret: %q", put.Account.WebhookPath)
	}
	if strings.Contains(rr.Body.String(), "secret-key-123456") {
		t.Errorf("secret echoed in the response")
	}
	// The service has the real values.
	a, _ := svc.ForTenant(context.Background(), "t1", integrations.ProviderCloudloop)
	if a == nil || a.Get("api_key") != "secret-key-123456" || a.Get("webhook_token") != put.Reveal["webhook_token"] {
		t.Fatalf("stored account: %+v", a)
	}

	rr = doReq(r, http.MethodGet, "/api/tenant/integrations", "")
	var list []map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &list)
	if rr.Code != http.StatusOK || len(list) != len(integrations.Specs) {
		t.Fatalf("list: %d %d", rr.Code, len(list))
	}
	if strings.Contains(rr.Body.String(), "secret-key-123456") {
		t.Errorf("secret in list")
	}
	if rr := doReq(r, http.MethodDelete, "/api/tenant/integrations/cloudloop", ""); rr.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rr.Code)
	}
	if a, _ := svc.ForTenant(context.Background(), "t1", integrations.ProviderCloudloop); a != nil {
		t.Errorf("still configured after delete")
	}
}

func TestTenantIntegrations_ViewerCannotWrite(t *testing.T) {
	r, _ := integrationsRouter(t, "t1", hubauth.RoleViewer)
	if rr := doReq(r, http.MethodPut, "/api/tenant/integrations/cloudloop", `{"values":{"api_key":"k"}}`); rr.Code != http.StatusForbidden {
		t.Fatalf("viewer put: %d", rr.Code)
	}
	if rr := doReq(r, http.MethodGet, "/api/tenant/integrations", ""); rr.Code != http.StatusOK {
		t.Fatalf("viewer list: %d", rr.Code)
	}
}
