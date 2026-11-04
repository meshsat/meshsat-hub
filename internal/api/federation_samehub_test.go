package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/federation"
	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
	"github.com/meshsat/meshsat-hub/internal/webhookroute"
)

// A tenant adds another tenant of the SAME Hub as a peer (MESHSAT-1788). The
// Hub's own name does not resolve in a test, as hub.meshsat.net resolves to a
// private address in the cluster; the guard is not disabled, so only this
// Hub's own receiving endpoint gets past it.
func TestCreatePeerAcceptsThisHubAndStillRefusesPrivateTargets(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(t.TempDir()+"/hub.db", 0)
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	accounts := integrations.New(db, make([]byte, 32))
	svc := federation.New(db, accounts, "https://hub.example.test", nil, nil)
	svc.SetLocalReceiver(webhookroute.Middleware(accounts, integrations.ProviderFederation, "relay_token"),
		func(ctx context.Context, token string) (string, error) {
			tenantID, _, err := accounts.LookupByToken(ctx, integrations.ProviderFederation, "relay_token", token)
			return tenantID, err
		})
	h := NewFederationHandler(db, svc, nil)

	now := time.Now().UTC()
	if err := db.CreateTenant(ctx, &store.Tenant{ID: "t-other", Slug: "t-other", Name: "t-other", Plan: "beta", Status: "active", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	other, otherURL, err := svc.Identity(ctx, "t-other")
	if err != nil {
		t.Fatal(err)
	}
	certJSON, _ := json.Marshal(string(other.CertPEM))
	add := func(relayURL string) *httptest.ResponseRecorder {
		body := `{"name":"peer","relay_url":"` + relayURL + `","certificate_pem":` + string(certJSON) + `}`
		r := httptest.NewRequest(http.MethodPost, "/api/federation/peers", strings.NewReader(body))
		r = r.WithContext(withTenant(r.Context(), "default"))
		w := httptest.NewRecorder()
		h.CreatePeer(w, r)
		return w
	}

	for _, u := range []string{
		"https://10.255.11.65/api/webhook/federation/tok",
		"https://hub.example.test:8443/api/webhook/federation/tok",
		"https://hub.example.test/api/webhook/federation/tok/../x",
		"https://hub.example.test.evil.test/api/webhook/federation/tok",
		// This Hub's endpoint, a token no tenant here holds.
		"https://hub.example.test/api/webhook/federation/tok",
	} {
		if w := add(u); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "relay_url") {
			t.Errorf("%s: want the guard's 400, got %d %s", u, w.Code, w.Body.String())
		}
	}
	if w := add(otherURL); w.Code != http.StatusCreated {
		t.Fatalf("a tenant of this Hub: %d %s", w.Code, w.Body.String())
	}
}
