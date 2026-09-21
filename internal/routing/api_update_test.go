package routing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// A route update is partial for EVERY field. Filter and senders used to be
// assigned unconditionally, so a request that changed only the name blanked
// them: for a recipient destination that is the recipient list and the list of
// who may trigger the route. It happened in production on 21 Sep 2026 to the
// two kit-to-kit SMS routes, which were left texting nobody and open to anyone.
func TestUpdatingOneFieldOfARouteLeavesTheOthersAlone(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(":memory:", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	route := &store.Route{ID: "r1", Name: "kit a to kit b", SourceType: "sms", DestinationType: "sms",
		Filter: "+31600000002", Senders: "+31600000001", Enabled: true}
	if err := db.CreateRoute(ctx, store.DefaultTenantID, route); err != nil {
		t.Fatal(err)
	}
	h := NewAPIHandler(db, &Engine{cachedRoutes: map[string]routeCache{}})

	put := func(body string) *store.Route {
		t.Helper()
		r := httptest.NewRequest(http.MethodPut, "/api/routes/r1", strings.NewReader(body))
		rc := chi.NewRouteContext()
		rc.URLParams.Add("id", "r1")
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc))
		w := httptest.NewRecorder()
		h.UpdateRoute(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT %s -> %d %s", body, w.Code, w.Body.String())
		}
		got, err := db.GetRoute(ctx, store.DefaultTenantID, "r1")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	got := put(`{"name":"renamed"}`)
	if got.Name != "renamed" {
		t.Errorf("name = %q", got.Name)
	}
	if got.Filter != "+31600000002" || got.Senders != "+31600000001" {
		t.Fatalf("a rename blanked the route: filter %q, senders %q", got.Filter, got.Senders)
	}
	if !got.Enabled || got.SourceType != "sms" || got.DestinationType != "sms" {
		t.Errorf("a rename changed something else: %+v", got)
	}

	// Explicitly empty still clears, which is how the UI removes a sender list.
	got = put(`{"senders":""}`)
	if got.Senders != "" || got.Filter != "+31600000002" {
		t.Errorf("clearing senders: filter %q, senders %q", got.Filter, got.Senders)
	}
	// And the whole object, as the UI sends it, still works.
	b, _ := json.Marshal(map[string]any{"name": "n", "source_type": "sms", "destination_type": "sms", "filter": "+31600000003", "senders": "+31600000001", "enabled": false})
	got = put(string(b))
	if got.Filter != "+31600000003" || got.Senders != "+31600000001" || got.Enabled {
		t.Errorf("full update: %+v", got)
	}
}
