package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	hubauth "github.com/meshsat/meshsat-hub/internal/auth"
	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/quota"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// The plan ceiling is the one place the Hub refuses a customer for money, and
// nothing wired a quota.Checker into a handler and asserted the 402 -- deleting
// the check from either handler broke no test at all. It has to hold on both
// paths, say something a person can act on, and register nothing when it
// refuses (MESHSAT-989).
func quotaRouter(t *testing.T, tenantID string) (http.Handler, store.Store) {
	t.Helper()
	s, err := sqlite.New(":memory:", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.CreateTenant(t.Context(), &store.Tenant{
		ID: tenantID, Slug: tenantID, Name: tenantID, Plan: plans.Free, Status: store.TenantActive,
	}); err != nil {
		t.Fatal(err)
	}

	q := quota.New(s, func(ctx context.Context, id string) (string, error) {
		tn, err := s.GetTenant(ctx, id)
		if err != nil || tn == nil {
			return plans.Free, err
		}
		return tn.Plan, nil
	})
	dh := NewDeviceHandler(s)
	dh.SetQuota(q)
	bh := NewBridgeHandler(s, nil)
	bh.SetQuota(q)

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := context.WithValue(req.Context(), hubauth.UserContextKey,
				&hubauth.User{ID: "u1", TenantID: tenantID, Roles: []string{"owner"}})
			ctx = context.WithValue(ctx, hubauth.TenantContextKey, tenantID)
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Post("/api/devices", dh.CreateDevice)
	r.Post("/api/bridges", bh.CreateBridge)
	return r, s
}

func TestTheFreeCeilingRefusesTheFifthRegistration(t *testing.T) {
	r, s := quotaRouter(t, "t-quota")
	post := func(path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}

	// The ceiling is devices AND bridges together, so mix them.
	for i := 1; i <= 3; i++ {
		if w := post("/api/devices", `{"imei":"30000000000000`+string(rune('0'+i))+`","label":"d"}`); w.Code != 200 && w.Code != 201 {
			t.Fatalf("device %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	if w := post("/api/bridges", `{"bridge_id":"br-1","label":"b"}`); w.Code != 200 && w.Code != 201 {
		t.Fatalf("bridge 4 of 4: %d %s", w.Code, w.Body.String())
	}

	// Five. Both paths must refuse, with the same 402.
	w := post("/api/devices", `{"imei":"300000000099999","label":"five"}`)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("5th device: %d %s, want 402", w.Code, w.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	for _, want := range []string{"free plan covers", "move up a plan"} {
		if !strings.Contains(body["error"], want) {
			t.Errorf("the refusal is not written for a person, missing %q: %q", want, body["error"])
		}
	}
	if w := post("/api/bridges", `{"bridge_id":"br-2","label":"five"}`); w.Code != http.StatusPaymentRequired {
		t.Errorf("5th bridge: %d %s, want 402", w.Code, w.Body.String())
	}

	// And a refusal registers nothing: a 402 that still wrote the row would
	// cap the tenant one lower every time somebody hit the wall.
	devs, err := s.ListDevices(t.Context(), "t-quota")
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 3 {
		t.Errorf("%d devices after a refused registration, want 3", len(devs))
	}
}

// A duplicate is a duplicate whatever the plan says. Re-adding a bridge the
// tenant already owns is not a new registration and must not be refused for
// money -- the 409 has to come first, or a tenant at its cap is told to pay to
// fix a mistake that costs us nothing.
func TestADuplicateIsRefusedBeforeThePlanIs(t *testing.T) {
	r, _ := quotaRouter(t, "t-dup")
	post := func(path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}
	for i := 1; i <= 4; i++ {
		if w := post("/api/bridges", `{"bridge_id":"br-`+string(rune('0'+i))+`","label":"b"}`); w.Code != 200 && w.Code != 201 {
			t.Fatalf("bridge %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	if w := post("/api/bridges", `{"bridge_id":"br-1","label":"b"}`); w.Code != http.StatusConflict {
		t.Errorf("re-adding an owned bridge at the cap: %d %s, want 409", w.Code, w.Body.String())
	}
}
