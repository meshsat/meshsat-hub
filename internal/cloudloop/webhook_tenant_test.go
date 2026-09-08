package cloudloop

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/dedup"
	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
	"github.com/meshsat/meshsat-hub/internal/webhookroute"
)

// Per-tenant webhook tokens (MESHSAT-977): the token selects the tenant, an
// unregistered device joins it, a device owned by another tenant is refused,
// and the platform token still maps to the default tenant.
func TestWebhookHandler_TenantTokens(t *testing.T) {
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().UTC()
	for _, id := range []string{"tenant-a", "tenant-b"} {
		if err := db.CreateTenant(ctx, &store.Tenant{ID: id, Slug: id, Name: id, Plan: "beta", Status: "active", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create tenant %s: %v", id, err)
		}
	}
	const imeiB = "300258060902280" // registered to tenant-b
	if err := db.CreateDevice(ctx, "tenant-b", &store.Device{IMEI: imeiB, Label: "b"}); err != nil {
		t.Fatalf("device: %v", err)
	}
	key := make([]byte, 32)
	accounts := integrations.New(db, key)
	accounts.SetPlatform(integrations.ProviderCloudloop, map[string]string{"api_key": "platform", "webhook_token": "platform-token"})
	acctA, _ := accounts.Set(ctx, "tenant-a", integrations.ProviderCloudloop, map[string]string{"api_key": "ka"})
	acctB, _ := accounts.Set(ctx, "tenant-b", integrations.ProviderCloudloop, map[string]string{"api_key": "kb"})

	h := NewWebhookHandler(&mockBus{})
	h.SetAllowedIPs([]string{"*"})
	h.SetAccounts(accounts)
	h.SetStore(db)
	h.SetTenants(tenancy.NewResolver(db, store.DefaultTenantID, time.Minute))
	dd := dedup.NewMemoryDedup(5 * time.Minute)
	defer dd.Close()
	h.SetDedup(dd)

	post := func(imei, token string) int {
		mo := newTestLingoMO(imei, "hello")
		mo.ID = "id-" + imei + "-" + token[:4]
		body, _ := json.Marshal(mo)
		req := httptest.NewRequest(http.MethodPost, "/api/webhook/cloudloop?token="+token, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	if c := post(imeiB, "unknown-token"); c != http.StatusUnauthorized {
		t.Errorf("unknown token: %d", c)
	}
	// tenant-b's token for tenant-b's device: stored in tenant-b.
	if c := post(imeiB, acctB.Get("webhook_token")); c != http.StatusOK {
		t.Errorf("own device: %d", c)
	}
	// tenant-a's token for tenant-b's device: refused.
	if c := post(imeiB, acctA.Get("webhook_token")); c != http.StatusForbidden {
		t.Errorf("foreign device: %d, want 403", c)
	}
	// tenant-a's token for an unknown device: joins tenant-a.
	const imeiNew = "300258060909999"
	if c := post(imeiNew, acctA.Get("webhook_token")); c != http.StatusOK {
		t.Errorf("new device: %d", c)
	}
	// platform token: default tenant.
	const imeiDef = "300258060900001"
	if c := post(imeiDef, "platform-token"); c != http.StatusOK {
		t.Errorf("platform token: %d", c)
	}
	count := func(tenant, imei string) int {
		ms, _ := db.ListMessages(ctx, tenant, imei, 10)
		return len(ms)
	}
	if count("tenant-b", imeiB) != 1 || count("tenant-a", imeiB) != 0 || count(store.DefaultTenantID, imeiB) != 0 {
		t.Errorf("imeiB rows: b=%d a=%d default=%d", count("tenant-b", imeiB), count("tenant-a", imeiB), count(store.DefaultTenantID, imeiB))
	}
	if count("tenant-a", imeiNew) != 1 || count(store.DefaultTenantID, imeiNew) != 0 {
		t.Errorf("imeiNew rows: a=%d default=%d", count("tenant-a", imeiNew), count(store.DefaultTenantID, imeiNew))
	}
	if count(store.DefaultTenantID, imeiDef) != 1 {
		t.Errorf("platform rows: %d", count(store.DefaultTenantID, imeiDef))
	}
}

// A request authenticated by the secret in its path must not then be judged by
// the IP allowlist branch. Restructuring the allowlist check for MESHSAT-975
// sent path-authenticated requests down the else branch, and a real webhook to
// a real tenant's URL came back 403 on the cluster while every unit test still
// passed. This is that case.
func TestWebhookHandler_PathTenantIsNotRefusedByAllowlist(t *testing.T) {
	for _, allowlist := range [][]string{{"*"}, {"203.0.113.7"}} {
		h := &WebhookHandler{allowedIPs: allowlist}
		ctx := webhookroute.NewContext(context.Background(), &webhookroute.Resolved{TenantID: "tenant-a"})
		req := httptest.NewRequest(http.MethodPost, "/api/webhook/cloudloop/s3cr3t",
			bytes.NewReader([]byte(`{"id":"x","identity":{},"message":""}`))).WithContext(ctx)
		req.RemoteAddr = "203.0.113.7:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusForbidden {
			t.Errorf("allowlist %v: path-authenticated request refused with 403", allowlist)
		}
	}
}
