package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/audit"
	"github.com/meshsat/meshsat-hub/internal/bridge"
	"github.com/meshsat/meshsat-hub/internal/directory"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// MESHSAT-1308. No bridge route wrote an audit entry: whether a bridge had
// ever been created over another tenant's (MESHSAT-1307) could not be told
// from the log, and the routes that mint a bridge's password and private key
// left no trace.

func auditStore(t *testing.T) *sqlite.DB {
	t.Helper()
	s, err := sqlite.New(":memory:", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func actions(t *testing.T, s store.Store, tenant string) map[string]store.AuditEntry {
	t.Helper()
	es, err := s.ListAuditEntries(context.Background(), tenant, 100)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.AuditEntry{}
	for _, e := range es {
		out[e.Action] = e
	}
	return out
}

func serve(r http.Handler, method, path, tenant, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(callerCtx(tenant, false))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestBridgeLifecycleIsAudited(t *testing.T) {
	s := auditStore(t)
	bh := NewBridgeHandler(s, nil)
	bh.SetAudit(audit.New(s))
	r := chi.NewRouter()
	r.Post("/api/bridges", bh.CreateBridge)
	r.Put("/api/bridges/{id}", bh.UpdateBridge)
	r.Delete("/api/bridges/{id}", bh.DeleteBridge)

	if w := serve(r, http.MethodPost, "/api/bridges", store.DefaultTenantID, `{"bridge_id":"kit-a","label":"Kit A"}`); w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	// Another tenant trying the same id: refused, recorded in ITS log only.
	if w := serve(r, http.MethodPost, "/api/bridges", "t-other", `{"bridge_id":"kit-a"}`); w.Code != http.StatusConflict {
		t.Fatalf("cross-tenant create: %d", w.Code)
	}
	if w := serve(r, http.MethodPut, "/api/bridges/kit-a", store.DefaultTenantID, `{"cot_callsign":"ALPHA"}`); w.Code != http.StatusOK {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	if w := serve(r, http.MethodDelete, "/api/bridges/kit-a", store.DefaultTenantID, ``); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}

	got := actions(t, s, store.DefaultTenantID)
	for action, want := range map[string]string{
		"bridge_created": `bridge=kit-a label="Kit A"`,
		"bridge_updated": `bridge=kit-a fields=cot_callsign="ALPHA"`,
		"bridge_deleted": `bridge=kit-a`,
	} {
		if e, ok := got[action]; !ok || e.Detail != want || e.Actor != "user-1" {
			t.Errorf("%s: %+v, want detail %q by user-1", action, e, want)
		}
	}
	if _, leaked := got["bridge_create_refused"]; leaked {
		t.Error("the other tenant's refused attempt landed in the victim's log")
	}
	if e, ok := actions(t, s, "t-other")["bridge_create_refused"]; !ok || !strings.Contains(e.Detail, "bridge=kit-a") {
		t.Errorf("the refused attempt is not in the caller's log: %+v", e)
	}
}

func TestMintingBridgeCredentialsIsAuditedWithoutTheSecret(t *testing.T) {
	s := auditStore(t)
	ctx := context.Background()
	if err := s.CreateOrUpdateBridge(ctx, store.DefaultTenantID, &store.Bridge{BridgeID: "kit-a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSystemConfig(ctx, mqttPublicURLKey, "wss://mqtt.test/mqtt"); err != nil {
		t.Fatal(err)
	}
	ca, _, _, err := bridge.NewSelfSignedCA("test")
	if err != nil {
		t.Fatal(err)
	}
	a := audit.New(s)
	ah := NewBridgeAuthHandler(s, ca)
	ah.SetAudit(a)
	anchor, err := directory.LoadOrCreateTrustAnchor(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	ph := NewBridgeProvisionHandler(s, ca, anchor)
	ph.SetAudit(a)
	r := chi.NewRouter()
	r.Post("/api/bridges/{id}/credentials", ah.GenerateCredentials)
	r.Post("/api/bridges/{id}/certificate", ah.IssueCertificate)
	r.Post("/api/bridges/{id}/provision/qr", ph.ProvisionQR)
	r.Get("/api/bridges/{id}/provision/{nonce}", ph.ClaimProvision)

	cred := serve(r, http.MethodPost, "/api/bridges/kit-a/credentials", store.DefaultTenantID, ``)
	if cred.Code != http.StatusOK {
		t.Fatalf("credentials: %d %s", cred.Code, cred.Body.String())
	}
	if w := serve(r, http.MethodPost, "/api/bridges/kit-a/certificate", store.DefaultTenantID, ``); w.Code != http.StatusOK {
		t.Fatalf("certificate: %d %s", w.Code, w.Body.String())
	}
	if w := serve(r, http.MethodPost, "/api/bridges/kit-a/provision/qr", store.DefaultTenantID, ``); w.Code != http.StatusOK {
		t.Fatalf("qr: %d %s", w.Code, w.Body.String())
	}
	raw, _ := s.GetSystemConfig(ctx, provisionStashKey(store.DefaultTenantID, "kit-a"))
	nonce := raw[strings.Index(raw, `"nonce":"`)+9:]
	nonce = nonce[:strings.IndexByte(nonce, '"')]
	claim := httptest.NewRequest(http.MethodGet, "/api/bridges/kit-a/provision/"+nonce, nil)
	cw := httptest.NewRecorder()
	r.ServeHTTP(cw, claim)
	if cw.Code != http.StatusOK {
		t.Fatalf("claim: %d", cw.Code)
	}

	got := actions(t, s, store.DefaultTenantID)
	for _, action := range []string{"bridge_credentials_issued", "bridge_certificate_issued", "bridge_provision_qr_generated", "bridge_provision_claimed"} {
		e, ok := got[action]
		if !ok || !strings.Contains(e.Detail, "bridge=kit-a") {
			t.Errorf("%s missing or unnamed: %+v", action, e)
			continue
		}
		for _, secret := range []string{nonce, "PRIVATE KEY", `"pass"`} {
			if strings.Contains(e.Detail, secret) {
				t.Errorf("%s carries a secret: %q", action, e.Detail)
			}
		}
	}
	if e := got["bridge_provision_claimed"]; e.Actor != "provisioning-claim" {
		t.Errorf("claim actor %q", e.Actor)
	}
}
