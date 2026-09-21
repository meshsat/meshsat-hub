package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1307. Creating a bridge whose id another tenant holds answered 201
// and rewrote that tenant's row. It is refused now, with the same answer as
// for an id the caller already has, so the response says nothing about whose
// it is. The default tenant is the victim.
func TestCreatingABridgeIDAnotherTenantHoldsIsRefused(t *testing.T) {
	r, s := quotaRouter(t, "t-intruder")
	if err := s.CreateOrUpdateBridge(t.Context(), store.DefaultTenantID, &store.Bridge{BridgeID: "kit-owned", Label: "victim"}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/bridges", strings.NewReader(`{"bridge_id":"kit-owned","label":"intruder"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "bridge already exists") {
		t.Fatalf("create over another tenant's id: %d %s", w.Code, w.Body.String())
	}
	b, err := s.GetBridge(t.Context(), store.DefaultTenantID, "kit-owned")
	if err != nil || b.Label != "victim" {
		t.Fatalf("the victim's bridge was rewritten: %+v %v", b, err)
	}
}
