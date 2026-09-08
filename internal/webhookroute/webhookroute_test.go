package webhookroute

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// newAccounts returns a service holding a platform account and one account per
// named tenant, each with its own generated webhook secret.
func newAccounts(t *testing.T, tenants ...string) (*integrations.Service, map[string]string) {
	t.Helper()
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().UTC()
	secrets := map[string]string{}
	svc := integrations.New(db, make([]byte, 32))
	svc.SetPlatform(integrations.ProviderCloudloop, map[string]string{"api_key": "platform", "webhook_token": "platform-secret"})
	secrets[store.DefaultTenantID] = "platform-secret"
	for _, id := range tenants {
		if err := db.CreateTenant(ctx, &store.Tenant{ID: id, Slug: id, Name: id, Plan: "beta", Status: "active", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create tenant %s: %v", id, err)
		}
		acct, err := svc.Set(ctx, id, integrations.ProviderCloudloop, map[string]string{"api_key": "k-" + id})
		if err != nil {
			t.Fatalf("account %s: %v", id, err)
		}
		secrets[id] = acct.Get("webhook_token")
		if secrets[id] == "" {
			t.Fatalf("tenant %s got no generated webhook token", id)
		}
	}
	return svc, secrets
}

// serve runs a request for /w/{secret} through the middleware and reports the
// status and whichever tenant reached the handler.
func serve(svc *integrations.Service, secret string) (int, string) {
	seen := ""
	r := chi.NewRouter()
	r.With(Middleware(svc, integrations.ProviderCloudloop, "webhook_token")).
		Post("/w/{"+URLParam+"}", func(w http.ResponseWriter, req *http.Request) {
			seen = TenantID(req.Context())
			// The middleware must also set the tenancy context the handlers read.
			if t := tenancy.FromContext(req.Context()); t != seen {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/w/"+secret, nil))
	return rec.Code, seen
}

// Each tenant's secret selects that tenant and nothing else, and an unknown
// secret is a 404 rather than a 401 so the endpoint cannot be used to test
// guesses (MESHSAT-975).
func TestMiddleware_ResolvesTenantFromPath(t *testing.T) {
	svc, secrets := newAccounts(t, "tenant-a", "tenant-b")

	tests := []struct {
		name     string
		secret   string
		wantCode int
		wantTid  string
	}{
		{"tenant a", secrets["tenant-a"], http.StatusOK, "tenant-a"},
		{"tenant b", secrets["tenant-b"], http.StatusOK, "tenant-b"},
		{"platform", secrets[store.DefaultTenantID], http.StatusOK, store.DefaultTenantID},
		{"unknown secret", "nope-not-a-secret", http.StatusNotFound, ""},
		{"another tenant's shape", "0123456789abcdef0123456789abcdef", http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, tid := serve(svc, tt.secret)
			if code != tt.wantCode {
				t.Errorf("status = %d, want %d", code, tt.wantCode)
			}
			if tid != tt.wantTid {
				t.Errorf("tenant = %q, want %q", tid, tt.wantTid)
			}
		})
	}
}

// Two tenants must never resolve to each other, which is the whole point of
// the route shape.
func TestMiddleware_SecretsDoNotCross(t *testing.T) {
	svc, secrets := newAccounts(t, "tenant-a", "tenant-b")
	if secrets["tenant-a"] == secrets["tenant-b"] {
		t.Fatal("generated secrets collided")
	}
	if _, tid := serve(svc, secrets["tenant-a"]); tid != "tenant-a" {
		t.Errorf("tenant-a secret resolved to %q", tid)
	}
	if _, tid := serve(svc, secrets["tenant-b"]); tid != "tenant-b" {
		t.Errorf("tenant-b secret resolved to %q", tid)
	}
}

// A handler that was reached without the middleware must see no tenant, so it
// refuses rather than inventing one.
func TestTenantID_EmptyWithoutMiddleware(t *testing.T) {
	if got := TenantID(context.Background()); got != "" {
		t.Errorf("TenantID on a bare context = %q, want empty", got)
	}
}

// A nil service means the feature is not wired; the route must not exist
// rather than fall open.
func TestMiddleware_NilServiceIsNotFound(t *testing.T) {
	if code, _ := serve(nil, "anything"); code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
}
