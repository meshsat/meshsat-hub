package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1303. The stash was keyed by bridge id alone, but a bridge id is
// unique only within a tenant: two tenants with a bridge of the same name
// shared one row, so a new QR in one tenant silently killed the other's. The
// default tenant is the victim here, per the tenancy tests' convention.
func TestTwoTenantsWithTheSameBridgeNameKeepTheirOwnQR(t *testing.T) {
	m, h := provisionFixture(t)
	victim := stashOnly(t, h, "kit-a") // default tenant
	other, err := h.generateAndStash(httptest.NewRequest(http.MethodPost, "/", nil), "kit-a", "t-other")
	if err != nil {
		t.Fatal(err)
	}
	if len(provisionStashKeys(m)) != 2 {
		t.Fatalf("stash rows: %v; the second tenant's QR replaced the first's", provisionStashKeys(m))
	}
	for _, tc := range []struct{ nonce, wantPrefix string }{{victim, "meshsat"}, {other, "meshsat/t-other"}} {
		rr := claimBundle(t, h, "kit-a", tc.nonce)
		if rr.Code != http.StatusOK {
			t.Fatalf("claim for %s: %d %s", tc.wantPrefix, rr.Code, rr.Body.String())
		}
		var b ProvisionBundle
		_ = json.Unmarshal(rr.Body.Bytes(), &b)
		if b.MQTTTopicPrefix != tc.wantPrefix {
			t.Fatalf("a nonce claimed another tenant's bundle: prefix %q, want %q", b.MQTTTopicPrefix, tc.wantPrefix)
		}
	}
}

// A QR issued before the change (old key, no tenant) keeps working until it
// expires.
func TestAQRFromBeforeTheChangeStillClaims(t *testing.T) {
	m, h := provisionFixture(t)
	nonce := stashOnly(t, h, "kit-a")
	raw := m.sysConfig[provisionStashKey(store.DefaultTenantID, "kit-a")]
	delete(m.sysConfig, provisionStashKey(store.DefaultTenantID, "kit-a"))
	m.sysConfig[legacyProvisionStashKey("kit-a")] = raw
	if rr := claimBundle(t, h, "kit-a", nonce); rr.Code != http.StatusOK {
		t.Fatalf("legacy claim: %d", rr.Code)
	}
	if m.sysConfig[legacyProvisionStashKey("kit-a")] != "" {
		t.Fatal("a claimed legacy stash was not blanked")
	}
}

// Deleting a bridge takes its unclaimed bundle (plaintext password and client
// private key) with it, instead of leaving it for the reaper's next pass.
func TestDeletingABridgeRemovesItsProvisioningStash(t *testing.T) {
	m, h := provisionFixture(t)
	_ = stashOnly(t, h, "kit-a")
	bh := NewBridgeHandler(m, nil)
	r := chi.NewRouter()
	r.Delete("/api/bridges/{id}", bh.DeleteBridge)
	req := httptest.NewRequest(http.MethodDelete, "/api/bridges/kit-a", nil)
	// no tenant in the context reads as the default tenant, the victim
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rr.Code, rr.Body.String())
	}
	if keys := provisionStashKeys(m); len(keys) != 0 {
		t.Fatalf("the deleted bridge's stash survived: %v", keys)
	}
}
