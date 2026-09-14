package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// fakeStore is the webhook slice of the persistence layer, in memory.
type fakeStore struct {
	mu       sync.Mutex
	rows     map[string][]store.WebhookConfig // tenant -> webhooks
	tenants  []string
	failSave bool
	// claims is the shared dispatch_claims table. Two dispatchers sharing one
	// fakeStore are two Hub replicas sharing one database, which is the only
	// way to test that a customer's endpoint is POSTed to once and not twice.
	claims   map[string]bool
	claimErr error
}

func newFakeStore(tenants ...string) *fakeStore {
	return &fakeStore{rows: map[string][]store.WebhookConfig{}, tenants: tenants,
		claims: map[string]bool{}}
}

func (f *fakeStore) ClaimOnce(_ context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return false, f.claimErr
	}
	if f.claims[key] {
		return false, nil
	}
	f.claims[key] = true
	return true, nil
}

func (f *fakeStore) ListTenants(context.Context) ([]store.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.Tenant, 0, len(f.tenants))
	for _, id := range f.tenants {
		out = append(out, store.Tenant{ID: id})
	}
	return out, nil
}

func (f *fakeStore) ListWebhooks(_ context.Context, tenantID string) ([]store.WebhookConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.WebhookConfig(nil), f.rows[tenantID]...), nil
}

func (f *fakeStore) SaveWebhook(_ context.Context, tenantID string, w *store.WebhookConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSave {
		return errors.New("database is down")
	}
	for i, existing := range f.rows[tenantID] {
		if existing.ID == w.ID {
			f.rows[tenantID][i] = *w
			return nil
		}
	}
	f.rows[tenantID] = append(f.rows[tenantID], *w)
	return nil
}

func (f *fakeStore) DeleteWebhook(_ context.Context, tenantID, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.rows[tenantID][:0]
	for _, w := range f.rows[tenantID] {
		if w.ID != id {
			kept = append(kept, w)
		}
	}
	f.rows[tenantID] = kept
	return nil
}

// asTenant builds a request the way auth.TenantMiddleware leaves it.
func asTenant(method, target, body, tenantID string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	return r.WithContext(context.WithValue(r.Context(), auth.TenantContextKey, tenantID))
}

// The one field that would otherwise let a customer subscribe to somebody
// else's traffic. A tenant_id in the request body must be ignored.
func TestAForgedTenantInTheBodyIsIgnored(t *testing.T) {
	fs := newFakeStore(tenantA, store.DefaultTenantID)
	d := NewDispatcher(nil)
	d.AllowLoopbackTargetsForTest() // the target policy has its own tests; this is about tenancy
	d.SetStore(fs)
	h := NewAPIHandler(d)

	body := `{"url":"https://hooks.example.com/x","events":["sos"],"enabled":true,"tenant_id":"` +
		store.DefaultTenantID + `"}`
	w := httptest.NewRecorder()
	h.CreateWebhook(w, asTenant("POST", "/api/webhooks", body, tenantA))

	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if got := d.ListWebhooks(store.DefaultTenantID); len(got) != 0 {
		t.Errorf("a webhook created by tenant A landed on the default tenant: %v.\n"+
			"The caller named the tenant in the body and was believed.", got)
	}
	if got := d.ListWebhooks(tenantA); len(got) != 1 {
		t.Errorf("tenant A has %d webhooks, want 1", len(got))
	}
}

// The API used to append to a slice and write nothing down, so a customer's
// webhooks vanished at the next rollout with no message.
func TestACreatedWebhookIsPersistedAndReloads(t *testing.T) {
	fs := newFakeStore(tenantA, tenantB)
	d := NewDispatcher(nil)
	d.AllowLoopbackTargetsForTest() // the target policy has its own tests; this is about tenancy
	d.SetStore(fs)
	h := NewAPIHandler(d)

	w := httptest.NewRecorder()
	h.CreateWebhook(w, asTenant("POST", "/api/webhooks",
		`{"url":"https://hooks.example.com/a","events":["mo"],"enabled":true}`, tenantA))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}

	// A fresh dispatcher, as a restarted replica would have.
	fresh := NewDispatcher(nil)
	fresh.SetStore(fs)
	if err := fresh.LoadAll(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	got := fresh.ListWebhooks(tenantA)
	if len(got) != 1 || got[0].URL != "https://hooks.example.com/a" {
		t.Fatalf("after a restart tenant A has %v, want its one webhook back", got)
	}
	if got[0].TenantID != tenantA {
		t.Errorf("the reloaded webhook belongs to %q, want %q", got[0].TenantID, tenantA)
	}
	// And the events survived the round trip through the store's []string.
	if len(got[0].Events) != 1 || got[0].Events[0] != EventMO {
		t.Errorf("events after reload: %v, want [mo]", got[0].Events)
	}
}

// A failed write must not report success. The customer would see a webhook in
// the list that disappears at the next rollout.
func TestCreateFailsLoudlyWhenTheDatabaseRefuses(t *testing.T) {
	fs := newFakeStore(tenantA)
	fs.failSave = true
	d := NewDispatcher(nil)
	d.AllowLoopbackTargetsForTest() // the target policy has its own tests; this is about tenancy
	d.SetStore(fs)
	h := NewAPIHandler(d)

	w := httptest.NewRecorder()
	h.CreateWebhook(w, asTenant("POST", "/api/webhooks",
		`{"url":"https://hooks.example.com/a","events":["mo"],"enabled":true}`, tenantA))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500 -- the save failed", w.Code)
	}
	if got := d.ListWebhooks(tenantA); len(got) != 0 {
		t.Errorf("the webhook was added to memory despite the failed write: %v", got)
	}
}

// Two tenants registering the SAME url must not collide. The id used to be
// "wh-" + url, which is the primary key of webhook_configs, and SaveWebhook
// does ON CONFLICT (id) DO UPDATE -- so the second tenant would have taken
// over the first tenant's row, url, secret and all.
func TestTwoTenantsMayRegisterTheSameURL(t *testing.T) {
	fs := newFakeStore(tenantA, tenantB)
	d := NewDispatcher(nil)
	d.AllowLoopbackTargetsForTest() // the target policy has its own tests; this is about tenancy
	d.SetStore(fs)
	h := NewAPIHandler(d)

	const body = `{"url":"https://shared.example.com/hook","events":["mo"],"enabled":true}`
	for _, tenant := range []string{tenantA, tenantB} {
		w := httptest.NewRecorder()
		h.CreateWebhook(w, asTenant("POST", "/api/webhooks", body, tenant))
		if w.Code != http.StatusCreated {
			t.Fatalf("create for %s: %d %s", tenant, w.Code, w.Body.String())
		}
	}
	a, b := d.ListWebhooks(tenantA), d.ListWebhooks(tenantB)
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("tenant A has %d and tenant B has %d webhooks, want one each", len(a), len(b))
	}
	if a[0].ID == b[0].ID {
		t.Errorf("both tenants got webhook id %q; one row, two owners", a[0].ID)
	}
}

// The listing endpoint must return the caller's own webhooks and no others.
func TestTheListEndpointIsScopedToTheCaller(t *testing.T) {
	d := NewDispatcher(nil)
	d.AddWebhook(WebhookConfig{ID: "mine", TenantID: tenantA, URL: "https://a.example", Enabled: true})
	d.AddWebhook(WebhookConfig{ID: "theirs", TenantID: store.DefaultTenantID, URL: "https://b.example", Enabled: true})
	h := NewAPIHandler(d)

	w := httptest.NewRecorder()
	h.ListWebhooks(w, asTenant("GET", "/api/webhooks", "", tenantA))

	var got []WebhookConfig
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if len(got) != 1 || got[0].ID != "mine" {
		t.Fatalf("the list returned %v; another tenant's webhook URL is in there", got)
	}
}

// Critical rule 4: request bodies go through the readJSON guards.
func TestTheRequestBodyIsValidated(t *testing.T) {
	d := NewDispatcher(nil)
	d.AllowLoopbackTargetsForTest()
	h := NewAPIHandler(d)

	for _, tc := range []struct{ name, body string }{
		{"unknown field", `{"url":"https://a.example","admin":true}`},
		{"two values", `{"url":"https://a.example"}{"url":"https://b.example"}`},
		{"malformed", `{"url":`},
	} {
		w := httptest.NewRecorder()
		h.CreateWebhook(w, asTenant("POST", "/api/webhooks", tc.body, tenantA))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", tc.name, w.Code)
		}
	}
}

// Deleting is scoped too: a delete naming another tenant's id must leave it
// alone. The victim is the default tenant, the shape the bug actually took.
func TestDeleteCannotReachAnotherTenant(t *testing.T) {
	fs := newFakeStore(store.DefaultTenantID, tenantB)
	d := NewDispatcher(nil)
	d.AllowLoopbackTargetsForTest() // the target policy has its own tests; this is about tenancy
	d.SetStore(fs)
	if err := d.Save(context.Background(), WebhookConfig{
		ID: "victim-hook", TenantID: store.DefaultTenantID, URL: "https://v.example", Enabled: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h := NewAPIHandler(d)

	r := asTenant("DELETE", "/api/webhooks/victim-hook", "", tenantB)
	r = withURLParam(r, "id", "victim-hook")
	w := httptest.NewRecorder()
	h.DeleteWebhook(w, r)

	if len(d.ListWebhooks(store.DefaultTenantID)) != 1 {
		t.Error("another tenant deleted the default tenant's webhook through the API")
	}
	rows, _ := fs.ListWebhooks(context.Background(), store.DefaultTenantID)
	if len(rows) != 1 {
		t.Error("another tenant deleted the default tenant's row from the database")
	}
}

// withURLParam attaches a chi URL parameter the way the router would.
func withURLParam(r *http.Request, key, value string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, value)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// Both of these were found by creating one webhook against production and
// reading what came back, after the tenancy fix had already shipped. Neither
// was visible in a unit test, because every test until now built the config in
// Go and never round-tripped it through the store or the router.

// The generated id goes into DELETE /api/webhooks/{id}, which is ONE chi path
// segment. An id built from the URL carries "https://" and every path
// separator, so the delete request 404s at the router and the webhook can never
// be removed.
func TestTheGeneratedIDIsAddressableInAURLPath(t *testing.T) {
	fs := newFakeStore(tenantA)
	d := NewDispatcher(nil)
	d.AllowLoopbackTargetsForTest()
	d.SetStore(fs)
	h := NewAPIHandler(d)

	w := httptest.NewRecorder()
	h.CreateWebhook(w, asTenant("POST", "/api/webhooks",
		`{"url":"https://hooks.example.com/a/b?x=1","events":["mo"],"enabled":true}`, tenantA))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	id := created["id"]

	if strings.ContainsAny(id, "/?#%") {
		t.Fatalf("the generated id %q contains a character that cannot survive one path "+
			"segment. DELETE /api/webhooks/{id} will 404 and the webhook is undeletable.", id)
	}

	// And prove it actually deletes through the handler, chi param and all.
	r := withURLParam(asTenant("DELETE", "/api/webhooks/"+id, "", tenantA), "id", id)
	dw := httptest.NewRecorder()
	h.DeleteWebhook(dw, r)
	if dw.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", dw.Code, dw.Body.String())
	}
	if got := d.ListWebhooks(tenantA); len(got) != 0 {
		t.Errorf("the webhook survived its own delete: %v", got)
	}
	rows, _ := fs.ListWebhooks(context.Background(), tenantA)
	if len(rows) != 0 {
		t.Errorf("the row survived its own delete: %v", rows)
	}
}

// Two webhooks created with the same URL, by the same tenant, must not be one
// row: the id is the primary key and SaveWebhook does ON CONFLICT DO UPDATE.
func TestTwoWebhooksOnTheSameURLAreTwoRows(t *testing.T) {
	fs := newFakeStore(tenantA)
	d := NewDispatcher(nil)
	d.AllowLoopbackTargetsForTest()
	d.SetStore(fs)
	h := NewAPIHandler(d)

	const body = `{"url":"https://hooks.example.com/same","events":["mo"],"enabled":true}`
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		h.CreateWebhook(w, asTenant("POST", "/api/webhooks", body, tenantA))
		if w.Code != http.StatusCreated {
			t.Fatalf("create %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	if got := d.ListWebhooks(tenantA); len(got) != 2 {
		t.Errorf("got %d webhooks, want 2 -- the second overwrote the first", len(got))
	}
}

// The retry and timeout policy has to be in the ROW, not only in the copy the
// dispatcher holds. TimeoutSec 0 becomes http.Client{Timeout: 0}, which is no
// timeout at all: a target that accepts the connection and never answers holds
// a goroutine for the life of the process.
func TestTheRetryAndTimeoutPolicySurvivesAReload(t *testing.T) {
	fs := newFakeStore(tenantA)
	d := NewDispatcher(nil)
	d.AllowLoopbackTargetsForTest()
	d.SetStore(fs)
	h := NewAPIHandler(d)

	w := httptest.NewRecorder()
	h.CreateWebhook(w, asTenant("POST", "/api/webhooks",
		`{"url":"https://hooks.example.com/a","events":["mo"],"enabled":true}`, tenantA))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}

	// What was actually written down.
	rows, _ := fs.ListWebhooks(context.Background(), tenantA)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].TimeoutSec == 0 || rows[0].MaxRetries == 0 {
		t.Errorf("the stored row has timeout_sec=%d max_retries=%d. A zero timeout is no "+
			"timeout: one unresponsive target then holds a goroutine forever.",
			rows[0].TimeoutSec, rows[0].MaxRetries)
	}

	// And what a restarted replica loads back.
	fresh := NewDispatcher(nil)
	fresh.SetStore(fs)
	if err := fresh.LoadAll(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	got := fresh.ListWebhooks(tenantA)
	if len(got) != 1 || got[0].TimeoutSec != 10 || got[0].MaxRetries != 3 {
		t.Errorf("after a reload: %+v, want timeout_sec 10 and max_retries 3", got)
	}
}

// A row already in the database with zeros -- written before this was fixed, or
// by hand -- must heal on load rather than delivering with no timeout.
func TestAStoredRowWithNoPolicyHealsOnLoad(t *testing.T) {
	fs := newFakeStore(tenantA)
	_ = fs.SaveWebhook(context.Background(), tenantA, &store.WebhookConfig{
		ID: "legacy", URL: "https://hooks.example.com/legacy",
		Events: []string{"mo"}, Enabled: true, // MaxRetries and TimeoutSec zero
	})

	d := NewDispatcher(nil)
	d.SetStore(fs)
	if err := d.LoadAll(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	got := d.ListWebhooks(tenantA)
	if len(got) != 1 || got[0].TimeoutSec != 10 || got[0].MaxRetries != 3 {
		t.Errorf("loaded %+v, want the defaults applied", got)
	}
}
