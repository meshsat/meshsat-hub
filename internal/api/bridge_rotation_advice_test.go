package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/audit"
	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/lxmfhub"
	"github.com/meshsat/meshsat-hub/internal/lxmfhub/lxmfhubtest"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1731. A bridge id with a '.' is refused at creation. A kit deleted
// while the workspace has a Reticulum access code may hold it, so the delete
// says so in the audit log and the Reticulum page offers a new code until the
// network name changes.
func TestDeletedKitAdvisesANewReticulumCode(t *testing.T) {
	ctx := t.Context()
	s := auditStore(t)
	svc := lxmfhub.New(lxmfhub.Config{}, lxmfhubtest.NewMemStore(store.DefaultTenantID), lxmfhubtest.NewMemAccounts(), nil)
	bh := NewBridgeHandler(s, nil)
	bh.SetAudit(audit.New(s))
	bh.SetLXMF(svc)
	lh := NewLXMFHandler(svc, s, "rns.example.org:4242")
	r := chi.NewRouter()
	r.Post("/api/bridges", bh.CreateBridge)
	r.Delete("/api/bridges/{id}", bh.DeleteBridge)
	r.Get("/api/lxmf/identity", lh.Identity)
	r.Post("/api/lxmf/access/rotate", lh.RotateAccess)
	advised := func() []string {
		t.Helper()
		rec := serve(r, http.MethodGet, "/api/lxmf/identity", store.DefaultTenantID, ``)
		return decodeLXMFIdentity(t, rec).RotationAdvised
	}

	if w := serve(r, http.MethodPost, "/api/bridges", store.DefaultTenantID, `{"bridge_id":"x.tak"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a dotted id answered %d", w.Code)
	}
	if _, err := s.LookupBridgeTenant(ctx, "x.tak"); err == nil {
		t.Fatal("the dotted id was stored")
	}
	for _, id := range []string{"kit-a", "kit-b", "kit-c"} {
		if w := serve(r, http.MethodPost, "/api/bridges", store.DefaultTenantID, `{"bridge_id":"`+id+`"}`); w.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", id, w.Code, w.Body.String())
		}
	}

	// No address yet: nothing to hold, nothing advised.
	if w := serve(r, http.MethodDelete, "/api/bridges/kit-a", store.DefaultTenantID, ``); w.Code != http.StatusNoContent {
		t.Fatalf("delete kit-a: %d", w.Code)
	}
	if _, ok := actions(t, s, store.DefaultTenantID)[auditRotationAdvised]; ok {
		t.Fatal("advised a new code for a workspace that has none")
	}

	id, err := svc.EnsureIdentity(ctx, store.DefaultTenantID, "owner@example.org")
	if err != nil {
		t.Fatal(err)
	}
	if got := advised(); len(got) != 0 {
		t.Fatalf("advised before any kit was deleted under this code: %v", got)
	}
	// A code outlives a switch to Public, so the advice does not depend on the mode.
	public := lxmfhub.AccessPublic
	if _, err := svc.UpdateIdentity(ctx, store.DefaultTenantID, "owner@example.org", lxmfhub.IdentityUpdate{AccessMode: &public}); err != nil {
		t.Fatal(err)
	}
	for _, kit := range []string{"kit-b", "kit-c"} {
		if w := serve(r, http.MethodDelete, "/api/bridges/"+kit, store.DefaultTenantID, ``); w.Code != http.StatusNoContent {
			t.Fatalf("delete %s: %d", kit, w.Code)
		}
	}
	es, err := s.ListAuditEntriesByAction(ctx, store.DefaultTenantID, []string{auditRotationAdvised}, 10)
	if err != nil || len(es) != 2 {
		t.Fatalf("audit entries: %d %v", len(es), err)
	}
	var details []string
	for _, e := range es {
		if e.Actor != "user-1" {
			t.Fatalf("audit entry by %q", e.Actor)
		}
		details = append(details, e.Detail)
	}
	slices.Sort(details)
	if want := []string{"bridge=kit-b network=" + id.IFACNetname, "bridge=kit-c network=" + id.IFACNetname}; !slices.Equal(details, want) {
		t.Fatalf("audit details %v, want %v", details, want)
	}
	got := advised()
	slices.Sort(got)
	if !slices.Equal(got, []string{"kit-b", "kit-c"}) {
		t.Fatalf("advised %v, want kit-b and kit-c", got)
	}

	// A new code: nothing deleted since.
	if w := serve(r, http.MethodPost, "/api/lxmf/access/rotate", store.DefaultTenantID, ``); w.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", w.Code, w.Body.String())
	} else if got := decodeLXMFIdentity(t, w).RotationAdvised; len(got) != 0 {
		t.Fatalf("still advised after the rotation: %v", got)
	}
	if got := advised(); len(got) != 0 {
		t.Fatalf("still advised after the rotation: %v", got)
	}
}

// The advice is read with the handler's store; a handler without one (the
// node's own tests) leaves it out rather than failing the page.
func TestRotationAdviceWithoutAStore(t *testing.T) {
	svc := lxmfhub.New(lxmfhub.Config{}, lxmfhubtest.NewMemStore(store.DefaultTenantID), lxmfhubtest.NewMemAccounts(), nil)
	if _, err := svc.EnsureIdentity(t.Context(), store.DefaultTenantID, "owner@example.org"); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	NewLXMFHandler(svc, nil, "").Identity(rec, lxmfAs("owner", http.MethodGet, "/api/lxmf/identity", ""))
	if rec.Code != http.StatusOK || decodeLXMFIdentity(t, rec).RotationAdvised != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// An address saved before Private existed has no code until the leader fills
// it in; a kit deleted meanwhile holds none, so nothing is advised.
func TestDeletedKitAdvisesNothingForAnAddressWithoutACode(t *testing.T) {
	ctx := t.Context()
	s := auditStore(t)
	accounts := lxmfhubtest.NewMemAccounts()
	svc := lxmfhub.New(lxmfhub.Config{}, lxmfhubtest.NewMemStore(store.DefaultTenantID), accounts, nil)
	if _, err := svc.EnsureIdentity(ctx, store.DefaultTenantID, "owner@example.org"); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.Set(ctx, store.DefaultTenantID, integrations.ProviderLXMF, map[string]string{"ifac_netname": "", "ifac_passphrase": ""}); err != nil {
		t.Fatal(err)
	}
	if id, err := svc.LoadIdentity(ctx, store.DefaultTenantID); err != nil || id.IFACNetname != "" {
		t.Fatalf("the setup did not blank the code: %v", err)
	}
	bh := NewBridgeHandler(s, nil)
	bh.SetAudit(audit.New(s))
	bh.SetLXMF(svc)
	r := chi.NewRouter()
	r.Post("/api/bridges", bh.CreateBridge)
	r.Delete("/api/bridges/{id}", bh.DeleteBridge)
	if w := serve(r, http.MethodPost, "/api/bridges", store.DefaultTenantID, `{"bridge_id":"kit-a"}`); w.Code != http.StatusCreated {
		t.Fatalf("create: %d", w.Code)
	}
	if w := serve(r, http.MethodDelete, "/api/bridges/kit-a", store.DefaultTenantID, ``); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}
	if e, ok := actions(t, s, store.DefaultTenantID)[auditRotationAdvised]; ok {
		t.Fatalf("advised a new code for an address without one: %+v", e)
	}
}
