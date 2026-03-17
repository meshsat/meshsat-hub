package constellation

import (
	"context"
	"testing"
)

// mockBackend for testing.
type mockBackend struct {
	name       string
	available  bool
	maxPayload int
	cost       float64
	sent       []string
}

func (m *mockBackend) Name() string                       { return m.name }
func (m *mockBackend) MaxPayload() int                    { return m.maxPayload }
func (m *mockBackend) CostPerMessage() float64            { return m.cost }
func (m *mockBackend) IsAvailable(_ context.Context) bool { return m.available }
func (m *mockBackend) Send(_ context.Context, deviceID string, _ []byte) (*SendResult, error) {
	m.sent = append(m.sent, deviceID)
	return &SendResult{ID: "test-1", Status: "queued"}, nil
}
func (m *mockBackend) CheckStatus(_ context.Context, _ string) (*SendResult, error) {
	return &SendResult{ID: "test-1", Status: "delivered"}, nil
}

func TestRouter_SingleBackend(t *testing.T) {
	r := NewRouter(StrategyAvailable)
	b := &mockBackend{name: "iridium", available: true, maxPayload: 340, cost: 0.05}
	r.Register(b)

	result, err := r.Send(context.Background(), "device-1", []byte("hello"))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if result.Status != "queued" {
		t.Errorf("status: %s", result.Status)
	}
	if len(b.sent) != 1 {
		t.Errorf("expected 1 send, got %d", len(b.sent))
	}
}

func TestRouter_CheapestStrategy(t *testing.T) {
	r := NewRouter(StrategyCheapest)
	expensive := &mockBackend{name: "iridium", available: true, maxPayload: 340, cost: 0.05}
	cheap := &mockBackend{name: "astrocast", available: true, maxPayload: 160, cost: 0.01}
	r.Register(expensive)
	r.Register(cheap)

	_, _ = r.Send(context.Background(), "device-1", []byte("hello"))

	if len(cheap.sent) != 1 {
		t.Errorf("cheapest backend should be selected, but got iridium=%d astrocast=%d", len(expensive.sent), len(cheap.sent))
	}
}

func TestRouter_PreferredStrategy(t *testing.T) {
	r := NewRouter(StrategyPreferred)
	ir := &mockBackend{name: "iridium", available: true, maxPayload: 340, cost: 0.05}
	ac := &mockBackend{name: "astrocast", available: true, maxPayload: 160, cost: 0.01}
	r.Register(ir)
	r.Register(ac)

	r.SetDevicePreference("device-1", "astrocast")
	_, _ = r.Send(context.Background(), "device-1", []byte("hello"))

	if len(ac.sent) != 1 {
		t.Errorf("preferred backend should be astrocast, got iridium=%d astrocast=%d", len(ir.sent), len(ac.sent))
	}
}

func TestRouter_PayloadTooLarge(t *testing.T) {
	r := NewRouter(StrategyAvailable)
	r.Register(&mockBackend{name: "astrocast", available: true, maxPayload: 10, cost: 0.01})

	_, err := r.Send(context.Background(), "device-1", make([]byte, 100))
	if err == nil {
		t.Error("expected error for oversized payload")
	}
}

func TestRouter_Unavailable(t *testing.T) {
	r := NewRouter(StrategyAvailable)
	r.Register(&mockBackend{name: "iridium", available: false, maxPayload: 340})

	_, err := r.Send(context.Background(), "device-1", []byte("hello"))
	if err == nil {
		t.Error("expected error when no backends available")
	}
}

func TestRouter_FallbackWhenPreferredUnavailable(t *testing.T) {
	r := NewRouter(StrategyPreferred)
	ir := &mockBackend{name: "iridium", available: true, maxPayload: 340, cost: 0.05}
	ac := &mockBackend{name: "astrocast", available: false, maxPayload: 160, cost: 0.01}
	r.Register(ir)
	r.Register(ac)

	r.SetDevicePreference("device-1", "astrocast")
	_, _ = r.Send(context.Background(), "device-1", []byte("hello"))

	// Astrocast unavailable, should fall back to iridium
	if len(ir.sent) != 1 {
		t.Errorf("should fall back to iridium, got iridium=%d", len(ir.sent))
	}
}

func TestRouter_NoBackends(t *testing.T) {
	r := NewRouter(StrategyAvailable)
	_, err := r.Send(context.Background(), "device-1", []byte("hello"))
	if err == nil {
		t.Error("expected error with no backends")
	}
}

func TestRouter_ListBackends(t *testing.T) {
	r := NewRouter(StrategyAvailable)
	r.Register(&mockBackend{name: "iridium", available: true, maxPayload: 340})
	r.Register(&mockBackend{name: "astrocast", available: true, maxPayload: 160})

	backends := r.ListBackends()
	if len(backends) != 2 {
		t.Errorf("expected 2 backends, got %d", len(backends))
	}
}
