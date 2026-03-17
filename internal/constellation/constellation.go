// Package constellation provides a unified satellite send interface
// supporting multiple satellite constellations (Iridium, Astrocast, etc.).
// Backend selection by cost, latency, availability, and device preference.
package constellation

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// Backend is the interface for a satellite constellation send provider.
type Backend interface {
	// Name returns the constellation name ("iridium", "astrocast").
	Name() string

	// Send transmits a payload to the device.
	Send(ctx context.Context, deviceID string, payload []byte) (*SendResult, error)

	// CheckStatus queries the delivery status of a previous send.
	CheckStatus(ctx context.Context, sendID string) (*SendResult, error)

	// IsAvailable returns true if the backend is operational.
	IsAvailable(ctx context.Context) bool

	// MaxPayload returns the maximum payload size in bytes.
	MaxPayload() int

	// CostPerMessage returns the approximate cost per message (for routing decisions).
	CostPerMessage() float64
}

// SendResult is the outcome of a satellite send operation.
type SendResult struct {
	ID     string `json:"id"`
	Status string `json:"status"` // "queued", "sent", "delivered", "failed"
	Error  string `json:"error,omitempty"`
}

// Strategy determines how to select a backend for a given send.
type Strategy string

const (
	StrategyCheapest  Strategy = "cheapest"  // lowest cost per message
	StrategyFastest   Strategy = "fastest"   // lowest latency (Iridium preferred)
	StrategyAvailable Strategy = "available" // first available backend
	StrategyPreferred Strategy = "preferred" // use device's preferred constellation
)

// Router selects the best backend for a send operation.
type Router struct {
	mu       sync.RWMutex
	backends map[string]Backend
	strategy Strategy
	// devicePrefs maps device IMEI to preferred constellation name
	devicePrefs map[string]string
}

// NewRouter creates a new constellation router.
func NewRouter(strategy Strategy) *Router {
	if strategy == "" {
		strategy = StrategyAvailable
	}
	return &Router{
		backends:    make(map[string]Backend),
		strategy:    strategy,
		devicePrefs: make(map[string]string),
	}
}

// Register adds a backend to the router.
func (r *Router) Register(b Backend) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.backends[b.Name()] = b
	slog.Info("constellation: registered backend", "name", b.Name(), "max_payload", b.MaxPayload())
}

// SetDevicePreference sets the preferred constellation for a device.
func (r *Router) SetDevicePreference(deviceID, constellation string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.devicePrefs[deviceID] = constellation
}

// Send routes a message to the best available backend.
func (r *Router) Send(ctx context.Context, deviceID string, payload []byte) (*SendResult, error) {
	backend, err := r.selectBackend(ctx, deviceID, len(payload))
	if err != nil {
		return nil, err
	}

	slog.Info("constellation: sending via", "backend", backend.Name(), "device", deviceID, "bytes", len(payload))
	return backend.Send(ctx, deviceID, payload)
}

// CheckStatus queries the delivery status on the named backend.
func (r *Router) CheckStatus(ctx context.Context, backend, sendID string) (*SendResult, error) {
	r.mu.RLock()
	b, ok := r.backends[backend]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown backend: %s", backend)
	}
	return b.CheckStatus(ctx, sendID)
}

// ListBackends returns the names of all registered backends.
func (r *Router) ListBackends() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.backends))
	for name := range r.backends {
		names = append(names, name)
	}
	return names
}

func (r *Router) selectBackend(ctx context.Context, deviceID string, payloadSize int) (Backend, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.backends) == 0 {
		return nil, fmt.Errorf("no constellation backends registered")
	}

	// Filter to backends that can handle the payload size and are available
	var candidates []Backend
	for _, b := range r.backends {
		if b.MaxPayload() > 0 && payloadSize > b.MaxPayload() {
			continue
		}
		if !b.IsAvailable(ctx) {
			continue
		}
		candidates = append(candidates, b)
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no available backends for payload size %d", payloadSize)
	}

	switch r.strategy {
	case StrategyPreferred:
		if pref, ok := r.devicePrefs[deviceID]; ok {
			for _, b := range candidates {
				if b.Name() == pref {
					return b, nil
				}
			}
		}
		// Fall through to first available
		return candidates[0], nil

	case StrategyCheapest:
		best := candidates[0]
		for _, b := range candidates[1:] {
			if b.CostPerMessage() < best.CostPerMessage() {
				best = b
			}
		}
		return best, nil

	case StrategyFastest:
		// Iridium has near-global coverage, prefer it for latency
		for _, b := range candidates {
			if b.Name() == "iridium" {
				return b, nil
			}
		}
		return candidates[0], nil

	default: // StrategyAvailable
		return candidates[0], nil
	}
}
