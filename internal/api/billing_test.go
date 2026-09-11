package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// The HTTP auth middleware puts the tenant under auth.TenantContextKey. An
// earlier version of this handler read tenancy.FromContext instead -- the key
// the MQTT and webhook paths use -- so every signed-in customer who pressed
// Subscribe got 403 "no tenant".
//
// It compiled, it passed review, and it took a live check against production to
// find. A unit test is cheaper.
func TestBillingResolvesTheTenantTheWayEveryOtherHandlerDoes(t *testing.T) {
	ms := &mockStore{tenant: &store.Tenant{ID: "t_acme", Plan: "free"}}
	h := NewBillingHandler(ms, nil, "https://hub.meshsat.net")
	h.SetPrices(map[string]string{"price_crew": "crew"})

	req := httptest.NewRequest(http.MethodPost, "/api/tenant/billing/checkout",
		strings.NewReader(`{"plan":"crew"}`))
	req = req.WithContext(context.WithValue(req.Context(), hubauth.TenantContextKey, "t_acme"))
	w := httptest.NewRecorder()
	h.Checkout(w, req)

	// The client is nil here, so the furthest this can get is "billing is not
	// configured". What it must NOT be is 403: that means the tenant was not
	// found in the context at all.
	if w.Code == http.StatusForbidden {
		t.Fatalf("got 403 %q -- the handler is reading the wrong context key, "+
			"so every real customer would be refused", strings.TrimSpace(w.Body.String()))
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d %q, want 503 (no Stripe client configured in this test)",
			w.Code, strings.TrimSpace(w.Body.String()))
	}
}

// A request with no tenant at all still has to be refused rather than reaching
// the payment provider with an empty id.
func TestBillingRefusesARequestWithNoTenant(t *testing.T) {
	ms := &mockStore{}
	h := NewBillingHandler(ms, nil, "https://hub.meshsat.net")
	h.SetPrices(map[string]string{"price_crew": "crew"})

	req := httptest.NewRequest(http.MethodPost, "/api/tenant/billing/portal", nil)
	w := httptest.NewRecorder()
	h.Portal(w, req)
	if w.Code == http.StatusOK {
		t.Fatal("a portal session was opened for a request carrying no tenant")
	}
}

// custom and beta are operator-set and unlimited. A customer must not be able
// to buy one by naming it, and the refusal happens before Stripe is called.
func TestACustomerCannotBuyAnOperatorSetTier(t *testing.T) {
	ms := &mockStore{tenant: &store.Tenant{ID: "t_acme", Plan: "free"}}
	h := NewBillingHandler(ms, nil, "https://hub.meshsat.net")
	h.SetPrices(map[string]string{"price_crew": "crew", "price_fleet": "fleet"})

	for _, plan := range []string{"custom", "beta", "free", "nonsense"} {
		req := httptest.NewRequest(http.MethodPost, "/api/tenant/billing/checkout",
			strings.NewReader(`{"plan":"`+plan+`"}`))
		req = req.WithContext(context.WithValue(req.Context(), hubauth.TenantContextKey, "t_acme"))
		w := httptest.NewRecorder()
		h.Checkout(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("plan %q got %d, want 400 -- it has no price and must be refused here",
				plan, w.Code)
		}
	}
}

// The public donation route is exempt from the auth chain, so TenantMiddleware
// never runs and there is nothing in the context to read. The handler must not
// go looking: a version that called h.tenant(r) here would always get nil, and
// the obvious "fix" is to start trusting a header the caller controls.
func TestThePublicDonationRouteNeedsNoTenant(t *testing.T) {
	ms := &mockStore{}
	h := NewBillingHandler(ms, nil, "https://hub.meshsat.net")
	h.SetDonationPrice("price_gift")

	req := httptest.NewRequest(http.MethodPost, "/api/donate", nil)
	w := httptest.NewRecorder()
	h.DonatePublic(w, req)

	if w.Code == http.StatusForbidden {
		t.Fatalf("got 403 %q -- a stranger has no tenant and that is the point of this route",
			strings.TrimSpace(w.Body.String()))
	}
	// The client is nil in this test, so the furthest it can get is 503.
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d %q, want 503 (no Stripe client configured in this test)",
			w.Code, strings.TrimSpace(w.Body.String()))
	}
}

// With no donation price configured there is no donation path, and that is a
// supported state rather than a crash.
func TestDonationsAreRefusedCleanlyWhenNoPriceIsConfigured(t *testing.T) {
	h := NewBillingHandler(&mockStore{}, nil, "https://hub.meshsat.net")
	// deliberately no SetDonationPrice

	req := httptest.NewRequest(http.MethodPost, "/api/donate", nil)
	w := httptest.NewRecorder()
	h.DonatePublic(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "donations are not configured") {
		t.Errorf("body = %q; it should say which thing is missing", strings.TrimSpace(w.Body.String()))
	}
}

// The signed-in route is the mirror image: it DOES require a tenant, because a
// donation from a customer should be attached to their account.
func TestTheSignedInDonationRouteRequiresATenant(t *testing.T) {
	h := NewBillingHandler(&mockStore{}, nil, "https://hub.meshsat.net")
	h.SetDonationPrice("price_gift")

	req := httptest.NewRequest(http.MethodPost, "/api/tenant/billing/donate", nil)
	w := httptest.NewRecorder()
	h.Donate(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("got %d, want 403 for a request carrying no tenant", w.Code)
	}
}
