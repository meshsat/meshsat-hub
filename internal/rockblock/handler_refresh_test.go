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

// A modem that changed hands inside the resolver's TTL must be answered from
// the store: the new owner's delivery would otherwise be refused as another
// tenant's, and a publish under the old owner would be dropped by the consumers
// (MESHSAT-1469).
func TestADeliveryReadsTheOwnerOfThisMoment(t *testing.T) {
	db, err := sqlite.New(":memory:", 0)
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	const newOwner = "tenant-b"
	if err := db.CreateTenant(ctx, &store.Tenant{ID: newOwner, Slug: newOwner, Name: newOwner, Plan: "beta", Status: "active"}); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	const imei = "300234065000003"
	if err := db.CreateDevice(ctx, store.DefaultTenantID, &store.Device{IMEI: imei, Label: "modem"}); err != nil {
		t.Fatalf("device: %v", err)
	}
	accounts := integrations.New(db, make([]byte, 32))
	acct, err := accounts.Set(ctx, newOwner, integrations.ProviderRockBLOCK, nil)
	if err != nil {
		t.Fatalf("account: %v", err)
	}

	resolver := tenancy.NewResolver(db, store.DefaultTenantID, time.Minute)
	if got := resolver.ForDevice(ctx, imei); got != store.DefaultTenantID {
		t.Fatalf("seeding the cache: %q", got)
	}
	if err := db.DeleteDevice(ctx, store.DefaultTenantID, imei); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := db.CreateDevice(ctx, newOwner, &store.Device{IMEI: imei, Label: "modem"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	h := NewHandler(nil, "platform-secret")
	h.SetTenants(resolver)
	h.SetAccounts(accounts)
	h.SetStore(db)

	form := url.Values{"imei": {imei}, "momsn": {"1"}, "data": {"68656c6c6f"}, "transmit_time": {"26-11-08 10:00:00"}}
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/rockblock?token="+url.QueryEscape(acct.Get("webhook_secret")),
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("the new owner's delivery: %d %s", rr.Code, rr.Body.String())
	}
	if msgs, _ := db.ListMessages(ctx, newOwner, imei, 10); len(msgs) != 1 {
		t.Errorf("the new owner has %d messages, want 1", len(msgs))
	}
}
