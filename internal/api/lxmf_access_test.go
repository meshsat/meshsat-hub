package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/lxmfhub"
	"github.com/meshsat/meshsat-hub/internal/lxmfhub/lxmfhubtest"
	"github.com/meshsat/meshsat-hub/internal/store"
)

func lxmfAs(role, method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	u := &hubauth.User{ID: "u-" + role, Email: role + "@example.org", Roles: []string{role}, TenantID: store.DefaultTenantID}
	return r.WithContext(context.WithValue(r.Context(), hubauth.UserContextKey, u))
}

func decodeLXMFIdentity(t *testing.T, rec *httptest.ResponseRecorder) lxmfIdentityResponse {
	t.Helper()
	var resp lxmfIdentityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%d %s: %v", rec.Code, rec.Body.String(), err)
	}
	return resp
}

// The passphrase admits to a tenant's Private Reticulum network, so the
// identity shows it to owners and operators and only says to a viewer that
// there is one; the mode is validated; rotation answers a new code
// (MESHSAT-1722).
func TestLXMFAccess_PassphraseByRoleModeAndRotation(t *testing.T) {
	ctx := context.Background()
	svc := lxmfhub.New(lxmfhub.Config{}, lxmfhubtest.NewMemStore(store.DefaultTenantID), lxmfhubtest.NewMemAccounts(), nil)
	created, err := svc.EnsureIdentity(ctx, store.DefaultTenantID, "owner@example.org")
	if err != nil {
		t.Fatal(err)
	}
	h := NewLXMFHandler(svc, nil, "rns.example.org:4242")

	for _, tc := range []struct {
		role string
		sees bool
	}{{hubauth.RoleViewer, false}, {hubauth.RoleOperator, true}, {hubauth.RoleOwner, true}} {
		rec := httptest.NewRecorder()
		h.Identity(rec, lxmfAs(tc.role, http.MethodGet, "/api/lxmf/identity", ""))
		resp := decodeLXMFIdentity(t, rec)
		if resp.AccessMode != lxmfhub.AccessPrivate || resp.IFACNetname != created.IFACNetname {
			t.Fatalf("%s: mode %q netname %q", tc.role, resp.AccessMode, resp.IFACNetname)
		}
		if got := resp.IFACPassphrase == created.IFACPassphrase; got != tc.sees || resp.IFACPassphraseHidden == tc.sees {
			t.Fatalf("%s: passphrase shown=%v hidden=%v, want shown=%v", tc.role, resp.IFACPassphrase != "", resp.IFACPassphraseHidden, tc.sees)
		}
	}

	rec := httptest.NewRecorder()
	h.UpdateIdentity(rec, lxmfAs(hubauth.RoleOwner, http.MethodPut, "/api/lxmf/identity", `{"access_mode":"everyone"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown mode answered %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.UpdateIdentity(rec, lxmfAs(hubauth.RoleOwner, http.MethodPut, "/api/lxmf/identity", `{"access_mode":"public"}`))
	if resp := decodeLXMFIdentity(t, rec); rec.Code != http.StatusOK || resp.AccessMode != lxmfhub.AccessPublic {
		t.Fatalf("public: %d %+v", rec.Code, resp)
	}

	rec = httptest.NewRecorder()
	h.RotateAccess(rec, lxmfAs(hubauth.RoleOwner, http.MethodPost, "/api/lxmf/access/rotate", ""))
	rotated := decodeLXMFIdentity(t, rec)
	if rec.Code != http.StatusOK || rotated.IFACPassphrase == "" || rotated.IFACPassphrase == created.IFACPassphrase || rotated.IFACNetname == created.IFACNetname {
		t.Fatalf("rotate: %d %+v", rec.Code, rotated)
	}
}
