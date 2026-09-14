package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/geo"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1118. The geofence endpoints had no tenant in them at all: the list
// returned every tenant's polygons and escalation chain ids, the delete removed
// any fence by id, and create never recorded an owner -- so geo.Fence.TenantID,
// which had existed all along, stayed empty on every fence in the process.
//
// The victim is store.DefaultTenantID throughout, for the reason spelled out in
// internal/geo/tenant_test.go: the bug had no tenant dimension, so a regression
// collapses onto the default tenant and two non-default tenants would miss it.

const (
	gfVictim = store.DefaultTenantID
	gfOther  = "t_beta"
)

func gfRequest(method, target, body, tenantID string, params map[string]string) (*http.Request, *httptest.ResponseRecorder) {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	ctx := context.WithValue(r.Context(), auth.TenantContextKey, tenantID)
	if len(params) > 0 {
		rctx := chi.NewRouteContext()
		for k, v := range params {
			rctx.URLParams.Add(k, v)
		}
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	}
	return r.WithContext(ctx), httptest.NewRecorder()
}

const gfBody = `{"name":"perimeter","polygon":[{"lat":0,"lon":0},{"lat":0,"lon":1},{"lat":1,"lon":1}],"trigger":"both","enabled":true}`

func TestCreateFenceRecordsTheCallersTenant(t *testing.T) {
	e := geo.NewEngine()
	h := NewGeofenceHandler(e)

	r, w := gfRequest("POST", "/api/geofences", gfBody, gfOther, nil)
	h.CreateFence(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}

	if got := e.ListFences(gfOther); len(got) != 1 {
		t.Fatalf("the creating tenant has %d fences, want 1", len(got))
	}
	if got := e.ListFences(gfVictim); len(got) != 0 {
		t.Errorf("the fence landed on the default tenant as well: %v. Without an owner "+
			"recorded, every fence in the process belongs to nobody and is visible to "+
			"whoever the listing happens to ask for.", got)
	}
}

// The owner comes from the session, never from the body.
func TestCreateFenceIgnoresAForgedTenantInTheBody(t *testing.T) {
	e := geo.NewEngine()
	h := NewGeofenceHandler(e)

	body := `{"name":"perimeter","polygon":[{"lat":0,"lon":0},{"lat":0,"lon":1},{"lat":1,"lon":1}],` +
		`"trigger":"both","enabled":true,"tenant_id":"` + gfVictim + `"}`
	r, w := gfRequest("POST", "/api/geofences", body, gfOther, nil)
	h.CreateFence(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}

	if got := e.ListFences(gfVictim); len(got) != 0 {
		t.Errorf("a fence created by another tenant landed on the default tenant: %v. "+
			"The caller named the tenant in the body and was believed.", got)
	}
}

func TestListFencesIsScopedToTheCaller(t *testing.T) {
	e := geo.NewEngine()
	e.AddFence(geo.Fence{ID: "victim-fence", TenantID: gfVictim, Name: "victim",
		Polygon: []geo.Point{{Lat: 0, Lon: 0}, {Lat: 0, Lon: 1}, {Lat: 1, Lon: 1}},
		Trigger: geo.TriggerBoth, ChainID: "chain-victim", Enabled: true})
	e.AddFence(geo.Fence{ID: "own-fence", TenantID: gfOther, Name: "own",
		Polygon: []geo.Point{{Lat: 5, Lon: 5}, {Lat: 5, Lon: 6}, {Lat: 6, Lon: 6}},
		Trigger: geo.TriggerBoth, ChainID: "chain-other", Enabled: true})
	h := NewGeofenceHandler(e)

	r, w := gfRequest("GET", "/api/geofences", "", gfOther, nil)
	h.ListFences(w, r)

	var got []geo.Fence
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	for _, f := range got {
		if f.ID == "victim-fence" {
			t.Fatalf("another tenant's fence is in the listing: %v.\n"+
				"A polygon is where that customer operates, and chain_id names their "+
				"escalation chain.", got)
		}
	}
	if len(got) != 1 {
		t.Errorf("the caller sees %d fences, want its own 1", len(got))
	}
}

func TestDeleteFenceCannotReachAnotherTenant(t *testing.T) {
	e := geo.NewEngine()
	e.AddFence(geo.Fence{ID: "shared-id", TenantID: gfVictim, Name: "victim",
		Polygon: []geo.Point{{Lat: 0, Lon: 0}, {Lat: 0, Lon: 1}, {Lat: 1, Lon: 1}},
		Trigger: geo.TriggerBoth, Enabled: true})
	h := NewGeofenceHandler(e)

	r, w := gfRequest("DELETE", "/api/geofences/shared-id", "", gfOther,
		map[string]string{"id": "shared-id"})
	h.DeleteFence(w, r)

	if len(e.ListFences(gfVictim)) != 1 {
		t.Errorf("another tenant deleted the default tenant's fence through the API (HTTP %d)",
			w.Code)
	}
}

// gfStore is enough of the persistence layer for the handler to save through.
type gfStore struct {
	rows map[string][]store.Geofence
}

func (g *gfStore) ListTenants(context.Context) ([]store.Tenant, error) { return nil, nil }
func (g *gfStore) ListGeofences(_ context.Context, tenantID string) ([]store.Geofence, error) {
	return g.rows[tenantID], nil
}
func (g *gfStore) SaveGeofence(_ context.Context, tenantID string, f *store.Geofence) error {
	if g.rows == nil {
		g.rows = map[string][]store.Geofence{}
	}
	g.rows[tenantID] = append(g.rows[tenantID], *f)
	return nil
}
func (g *gfStore) DeleteGeofence(_ context.Context, tenantID, id string) error {
	kept := g.rows[tenantID][:0]
	for _, f := range g.rows[tenantID] {
		if f.ID != id {
			kept = append(kept, f)
		}
	}
	g.rows[tenantID] = kept
	return nil
}

// Creating a fence has to WRITE it, not just arm it in memory (MESHSAT-1119).
// A fence that exists only in a map is gone at the next rollout, and the
// customer cannot tell that from the Hub having forgotten it on purpose.
func TestCreateFencePersistsIt(t *testing.T) {
	st := &gfStore{}
	e := geo.NewEngine()
	e.SetStore(st)
	h := NewGeofenceHandler(e)

	r, w := gfRequest("POST", "/api/geofences", gfBody, gfOther, nil)
	h.CreateFence(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}

	rows, _ := st.ListGeofences(context.Background(), gfOther)
	if len(rows) != 1 {
		t.Fatalf("the database has %d fences after a create, want 1", len(rows))
	}
	if rows[0].TenantID != gfOther {
		t.Errorf("the stored row belongs to %q, want %q", rows[0].TenantID, gfOther)
	}
	if len(rows[0].Polygon) != 3 {
		t.Errorf("the stored polygon has %d vertices, want 3", len(rows[0].Polygon))
	}
}

// And deleting removes the row, or the fence returns at the next restart.
func TestDeleteFenceRemovesTheRow(t *testing.T) {
	st := &gfStore{}
	e := geo.NewEngine()
	e.SetStore(st)
	h := NewGeofenceHandler(e)

	r, w := gfRequest("POST", "/api/geofences", gfBody, gfOther, nil)
	h.CreateFence(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d", w.Code)
	}
	var created geo.Fence
	_ = json.Unmarshal(w.Body.Bytes(), &created)

	r, w = gfRequest("DELETE", "/api/geofences/"+created.ID, "", gfOther,
		map[string]string{"id": created.ID})
	h.DeleteFence(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	rows, _ := st.ListGeofences(context.Background(), gfOther)
	if len(rows) != 0 {
		t.Errorf("the row survived the delete: %v", rows)
	}
}

// --- crossing cooldown (MESHSAT-1119 follow-up) ---

func gfHandlerWithPolicy() (*geo.Engine, *GeofenceHandler) {
	e := geo.NewEngine()
	h := NewGeofenceHandler(e)
	h.SetCooldownPolicy(300, 30, 86400)
	return e, h
}

// The form needs the number actually in force, not one it hardcoded and that
// drifts from the ConfigMap.
func TestTheGeofencePolicyIsPublished(t *testing.T) {
	_, h := gfHandlerWithPolicy()
	r, w := gfRequest("GET", "/api/geofences/policy", "", gfOther, nil)
	h.Policy(w, r)

	var got map[string]int
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if got["cooldown_default"] != 300 || got["cooldown_min"] != 30 || got["cooldown_max"] != 86400 {
		t.Errorf("policy = %v, want 300/30..86400", got)
	}
}

func TestTheCooldownIsBoundedByThePlatform(t *testing.T) {
	e, h := gfHandlerWithPolicy()

	body := func(secs string) string {
		return `{"name":"perimeter","polygon":[{"lat":0,"lon":0},{"lat":0,"lon":1},{"lat":1,"lon":1}],` +
			`"trigger":"both","enabled":true,"cooldown_sec":` + secs + `}`
	}
	for _, secs := range []string{"5", "999999", "-1"} {
		r, w := gfRequest("POST", "/api/geofences", body(secs), gfOther, nil)
		h.CreateFence(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("cooldown_sec %s: got %d, want 400", secs, w.Code)
		}
	}
	if got := e.ListFences(gfOther); len(got) != 0 {
		t.Errorf("a refused fence was created anyway: %v", got)
	}

	// 0 is "use the platform default" and is always allowed; so is a value
	// inside the bounds.
	for _, secs := range []string{"0", "600"} {
		r, w := gfRequest("POST", "/api/geofences", body(secs), gfOther, nil)
		h.CreateFence(w, r)
		if w.Code != http.StatusCreated {
			t.Errorf("cooldown_sec %s: got %d %s, want 201", secs, w.Code, w.Body.String())
		}
	}
}

// The value has to survive to the engine, or the fence quietly runs on the
// platform default and the owner's choice did nothing.
func TestTheChosenCooldownReachesTheFence(t *testing.T) {
	e, h := gfHandlerWithPolicy()
	body := `{"name":"perimeter","polygon":[{"lat":0,"lon":0},{"lat":0,"lon":1},{"lat":1,"lon":1}],` +
		`"trigger":"both","enabled":true,"cooldown_sec":600}`
	r, w := gfRequest("POST", "/api/geofences", body, gfOther, nil)
	h.CreateFence(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	got := e.ListFences(gfOther)
	if len(got) != 1 || got[0].CooldownSec != 600 {
		t.Errorf("the fence carries %v, want cooldown_sec 600", got)
	}
}
