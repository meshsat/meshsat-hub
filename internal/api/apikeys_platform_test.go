package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1209. An API key may now carry PlatformAdmin, which is the axis that
// lets a caller act across every tenant. POST /api/auth/keys is gated at
// RequireRole(owner), so ANY tenant owner reaches CreateKey — which means the
// field is one missing check away from letting a customer mint themselves
// authority over the whole platform.
//
// That is the same shape as the defect this programme started with: a route
// handing out strictly more privilege than its gate implied, in that case a
// bridge private key to a tenant viewer. So the refusal is the feature here, and
// these tests are the reason the field is safe to exist at all.
//
// ⚠ Do not weaken these. If the escalation test ever needs changing to pass, the
// change is wrong.

func callerCtx(tid string, platformAdmin bool) context.Context {
	ctx := context.WithValue(context.Background(), auth.TenantContextKey, tid)
	return context.WithValue(ctx, auth.UserContextKey, &auth.User{
		ID:            "user-1",
		Roles:         []string{auth.RoleOwner},
		TenantID:      tid,
		PlatformAdmin: platformAdmin,
	})
}

func postKey(t *testing.T, ctx context.Context, body string, st *mockStore) *httptest.ResponseRecorder {
	t.Helper()
	h := NewAPIKeyHandler(st)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/keys", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.CreateKey(rec, req)
	return rec
}

// The escalation. A tenant owner — the highest role this route admits — asks for
// the platform axis and must be refused.
func TestATenantOwnerCannotMintAPlatformAdminKey(t *testing.T) {
	created := false
	st := &mockStore{createKeyFn: func(_ context.Context, _ string, k *store.APIKey) error {
		created = true
		k.ID = "key-escalated"
		return nil
	}}

	rec := postKey(t, callerCtx("t-customer", false), `{"label":"pwn","role":"owner","platform_admin":true}`, st)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("a tenant owner minted a platform-admin key: status = %d, want 403; body: %s",
			rec.Code, rec.Body.String())
	}
	// The store must not have been touched: a refusal that still writes the row
	// would leave a usable credential behind whatever the response said.
	if created {
		t.Fatal("CROSS-TENANT ESCALATION: the key was persisted despite the 403")
	}
}

// ...and with no caller in context at all, which is the shape a future
// refactor that drops the middleware would produce.
func TestAnUnidentifiedCallerCannotMintAPlatformAdminKey(t *testing.T) {
	created := false
	st := &mockStore{createKeyFn: func(_ context.Context, _ string, k *store.APIKey) error {
		created = true
		return nil
	}}
	ctx := context.WithValue(context.Background(), auth.TenantContextKey, "t-customer")

	rec := postKey(t, ctx, `{"label":"pwn","platform_admin":true}`, st)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("no user in context still minted a platform-admin key: status = %d, want 403", rec.Code)
	}
	if created {
		t.Fatal("CROSS-TENANT ESCALATION: persisted with no caller identity")
	}
}

// The positive control. Without this the refusals above are indistinguishable
// from a handler that rejects the field unconditionally, i.e. a feature that
// does not work.
func TestAPlatformAdminCanMintAPlatformAdminKey(t *testing.T) {
	var got *store.APIKey
	st := &mockStore{createKeyFn: func(_ context.Context, _ string, k *store.APIKey) error {
		k.ID = "key-platform"
		got = k
		return nil
	}}

	rec := postKey(t, callerCtx(store.DefaultTenantID, true), `{"label":"hub-verify","role":"owner","platform_admin":true}`, st)

	if rec.Code != http.StatusCreated {
		t.Fatalf("a platform admin was refused: status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	if got == nil || !got.PlatformAdmin {
		t.Fatalf("the stored key does not carry the axis it was granted: %+v", got)
	}
}

// And the ordinary path must never acquire it by accident — the field absent
// from the body has to mean false, not "whatever the zero value of the struct
// the caller reused happened to be".
func TestAnOrdinaryKeyNeverCarriesPlatformAdmin(t *testing.T) {
	var got *store.APIKey
	st := &mockStore{createKeyFn: func(_ context.Context, _ string, k *store.APIKey) error {
		k.ID = "key-ordinary"
		got = k
		return nil
	}}

	rec := postKey(t, callerCtx("t-customer", false), `{"label":"ordinary","role":"operator"}`, st)

	if rec.Code != http.StatusCreated {
		t.Fatalf("an ordinary key request failed: status = %d; body: %s", rec.Code, rec.Body.String())
	}
	if got == nil || got.PlatformAdmin {
		t.Fatalf("an ordinary key acquired the platform axis: %+v", got)
	}
	var resp createKeyResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Key == "" {
		t.Fatal("no plaintext key returned")
	}
}
