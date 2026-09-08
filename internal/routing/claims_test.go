package routing

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/sqlite"
)

// TestTwoEnginesOneStoreDispatchOnce is the MESHSAT-711 regression: two Hub
// replicas (two engines on one store) receive the same mo/decoded message
// and the matched route's handler runs exactly once.
func TestTwoEnginesOneStoreDispatchOnce(t *testing.T) {
	s, err := sqlite.New(":memory:", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	route := &store.Route{Name: "relay", SourceType: "*", DestinationType: "webhook", Filter: "", Enabled: true}
	if err := s.CreateRoute(ctx, store.DefaultTenantID, route); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	handler := func(_ context.Context, _ *store.Route, _ string, _ json.RawMessage) { calls.Add(1) }
	e1 := NewEngine(s, nil, nil)
	e2 := NewEngine(s, nil, nil)
	e1.RegisterHandler("webhook", handler)
	e2.RegisterHandler("webhook", handler)

	topic := "meshsat/300234063904190/mo/decoded"
	payload := []byte(`{"id":"mo-300234063904190-5","imei":"300234063904190","channel":"iridium","text":"hello"}`)
	e1.handleMODecoded(topic, payload)
	e2.handleMODecoded(topic, payload)
	if calls.Load() != 1 {
		t.Fatalf("expected exactly 1 dispatch across two replicas, got %d", calls.Load())
	}

	// A different message dispatches again; the same message without an id
	// still collapses through the topic+payload fallback.
	e2.handleMODecoded(topic, []byte(`{"imei":"300234063904190","channel":"iridium","text":"second"}`))
	e1.handleMODecoded(topic, []byte(`{"imei":"300234063904190","channel":"iridium","text":"second"}`))
	if calls.Load() != 2 {
		t.Fatalf("expected 2 dispatches after a second message, got %d", calls.Load())
	}
}
