package routing

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// TestRoutesEvaluatedForTheDeviceTenant: a device registered in tenant B is
// routed with tenant B's rules, an unregistered device with the default
// tenant's rules, and neither sees the other's routes (MESHSAT-864 MR 19).
func TestRoutesEvaluatedForTheDeviceTenant(t *testing.T) {
	s, err := sqlite.New(":memory:", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTenant(ctx, &store.Tenant{ID: "t_b", Slug: "b", Name: "B"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRoute(ctx, store.DefaultTenantID, &store.Route{Name: "default-webhook", SourceType: "*", DestinationType: "webhook", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRoute(ctx, "t_b", &store.Route{Name: "b-tak", SourceType: "*", DestinationType: "tak", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateDevice(ctx, "t_b", &store.Device{IMEI: "300234060000002", Label: "B device", Type: "rockblock"}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	seen := map[string][]string{} // route name → tenants from ctx
	e := NewEngine(s, nil, tenancy.NewResolver(s, store.DefaultTenantID, 0))
	for _, dest := range []string{"webhook", "tak"} {
		e.RegisterHandler(dest, func(ctx context.Context, r *store.Route, _ string, _ json.RawMessage) {
			mu.Lock()
			seen[r.Name] = append(seen[r.Name], tenancy.FromContext(ctx))
			mu.Unlock()
		})
	}

	e.handleMODecoded("meshsat/300234060000002/mo/decoded", []byte(`{"id":"m1","imei":"300234060000002","channel":"iridium","text":"from b"}`))
	e.handleMODecoded("meshsat/300234060000009/mo/decoded", []byte(`{"id":"m2","imei":"300234060000009","channel":"iridium","text":"unknown device"}`))

	mu.Lock()
	defer mu.Unlock()
	if got := seen["b-tak"]; len(got) != 1 || got[0] != "t_b" {
		t.Errorf("tenant B route: %v", got)
	}
	if got := seen["default-webhook"]; len(got) != 1 || got[0] != store.DefaultTenantID {
		t.Errorf("default route: %v", got)
	}
}
