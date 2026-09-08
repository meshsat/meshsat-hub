package rockblock

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// Per-tenant RockBLOCK webhook secrets (MESHSAT-977): ?token= selects the
// tenant, a foreign device is refused, the platform path keeps its secret.
func TestHandler_TenantTokens(t *testing.T) {
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
			t.Fatalf("tenant: %v", err)
		}
	}
	const imeiB = "300234065000002"
	if err := db.CreateDevice(ctx, "tenant-b", &store.Device{IMEI: imeiB, Label: "b"}); err != nil {
		t.Fatalf("device: %v", err)
	}
	accounts := integrations.New(db, make([]byte, 32))
	acctA, _ := accounts.Set(ctx, "tenant-a", integrations.ProviderRockBLOCK, nil)

	h := NewHandler(nil, "platform-secret")
	h.SetTenants(tenancy.NewResolver(db, store.DefaultTenantID, time.Minute))
	h.SetAccounts(accounts)
	h.SetStore(db)

	post := func(imei, token, jwt string) int {
		form := url.Values{"imei": {imei}, "momsn": {"1"}, "data": {"68656c6c6f"}, "transmit_time": {"26-09-08 10:00:00"}}
		if jwt != "" {
			form.Set("JWT", jwt)
		}
		target := "/api/webhook/rockblock"
		if token != "" {
			target += "?token=" + url.QueryEscape(token)
		}
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	if c := post(imeiB, "", ""); c != http.StatusUnauthorized {
		t.Errorf("platform path without secret: %d", c)
	}
	if c := post(imeiB, "", "platform-secret"); c != http.StatusOK {
		t.Errorf("platform JWT: %d", c)
	}
	if c := post(imeiB, "nope", ""); c != http.StatusUnauthorized {
		t.Errorf("unknown token: %d", c)
	}
	if c := post(imeiB, acctA.Get("webhook_secret"), ""); c != http.StatusForbidden {
		t.Errorf("tenant-a token for tenant-b device: %d, want 403", c)
	}
	const imeiNew = "300234065000009"
	if c := post(imeiNew, acctA.Get("webhook_secret"), ""); c != http.StatusOK {
		t.Errorf("tenant-a token for a new device: %d", c)
	}
	msgs, _ := db.ListMessages(ctx, "tenant-a", imeiNew, 10)
	def, _ := db.ListMessages(ctx, store.DefaultTenantID, imeiNew, 10)
	if len(msgs) != 1 || len(def) != 0 {
		t.Errorf("rows: tenant-a=%d default=%d", len(msgs), len(def))
	}
}
