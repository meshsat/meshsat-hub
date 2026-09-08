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
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

func postMO(h *WebhookHandler, url string, header map[string]string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(newTestLingoMO("300258060902280", "hello"))
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// A wildcard allowlist is only usable together with the shared token (MESHSAT-971).
func TestWebhookHandler_WildcardAllowlistRequiresToken(t *testing.T) {
	h := NewWebhookHandler(&mockBus{})
	h.SetAllowedIPs([]string{"*"})
	if rr := postMO(h, "/api/webhook/cloudloop", nil); rr.Code != http.StatusForbidden {
		t.Fatalf("no token configured: got %d, want 403", rr.Code)
	}
	h.SetToken("s3cret")
	if rr := postMO(h, "/api/webhook/cloudloop", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("token missing: got %d, want 401", rr.Code)
	}
	if rr := postMO(h, "/api/webhook/cloudloop?token=wrong", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", rr.Code)
	}
	if rr := postMO(h, "/api/webhook/cloudloop?token=s3cret", nil); rr.Code != http.StatusOK {
		t.Fatalf("query token: got %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	if rr := postMO(h, "/api/webhook/cloudloop", map[string]string{"X-Webhook-Token": "s3cret"}); rr.Code != http.StatusOK {
		t.Fatalf("header token: got %d, want 200", rr.Code)
	}
	// An explicit allowlist keeps working without a token.
	h2 := newTestWebhookHandler(&mockBus{})
	if rr := postMO(h2, "/api/webhook/cloudloop", nil); rr.Code != http.StatusOK {
		t.Fatalf("allowlisted IP without token: got %d, want 200", rr.Code)
	}
}

// The message row lands in the tenant that owns the device, not in the default
// tenant the auth-exempt webhook context falls back to (MESHSAT-975).
func TestWebhookHandler_PersistsInDeviceTenant(t *testing.T) {
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	const imei = "300258060902280"
	if err := db.CreateDevice(ctx, "tenant-b", &store.Device{IMEI: imei, Label: "b-device"}); err != nil {
		t.Fatalf("create device: %v", err)
	}

	h := newTestWebhookHandler(&mockBus{})
	h.SetStore(db)
	h.SetTenants(tenancy.NewResolver(db, store.DefaultTenantID, time.Minute))
	dd := dedup.NewMemoryDedup(5 * time.Minute)
	defer dd.Close()
	h.SetDedup(dd)

	if rr := postMO(h, "/api/webhook/cloudloop", nil); rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	inB, err := db.ListMessages(ctx, "tenant-b", imei, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	inDefault, _ := db.ListMessages(ctx, store.DefaultTenantID, imei, 10)
	if len(inB) != 1 || len(inDefault) != 0 {
		t.Fatalf("messages: tenant-b=%d default=%d, want 1/0", len(inB), len(inDefault))
	}
}
