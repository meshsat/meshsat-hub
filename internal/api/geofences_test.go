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
