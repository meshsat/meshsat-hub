package geo

import (
	"context"
	"log/slog"
	"net/url"
	"sync"
	"time"
)

// TriggerMode defines when a geofence triggers.
type TriggerMode string

const (
	TriggerEnter TriggerMode = "enter"
	TriggerExit  TriggerMode = "exit"
	TriggerBoth  TriggerMode = "both"
)

// Fence defines a polygon geofence with trigger configuration.
type Fence struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Polygon  []Point     `json:"polygon"`  // ordered vertices (closed polygon)
	Trigger  TriggerMode `json:"trigger"`  // "enter", "exit", "both"
	ChainID  string      `json:"chain_id"` // escalation chain to trigger
	Enabled  bool        `json:"enabled"`
	TenantID string      `json:"tenant_id,omitempty"`
}

// FenceEvent is emitted when a device crosses a geofence boundary.
type FenceEvent struct {
	// TenantID is the tenant that owns both the fence and the device. A fence
	// event raises an escalation chain, and a chain belongs to a tenant, so an
	// event that cannot say whose it is cannot be acted on.
	TenantID   string    `json:"tenant_id,omitempty"`
	FenceID    string    `json:"fence_id"`
	FenceName  string    `json:"fence_name"`
	DeviceIMEI string    `json:"device_imei"`
	EventType  string    `json:"event_type"` // "enter" or "exit"
	Lat        float64   `json:"lat"`
	Lon        float64   `json:"lon"`
	Timestamp  time.Time `json:"timestamp"`
}

// EventHandler is called when a geofence event occurs.
type EventHandler func(ctx context.Context, event FenceEvent)

// Engine evaluates device positions against configured geofences.
//
// MESHSAT-1118: every map here was keyed without a tenant. ListFences returned
// every tenant's fences -- a fence polygon is where a customer operates, which
// is not a thing to hand to another customer -- RemoveFence deleted any fence by
// id, and Evaluate checked a device against EVERY tenant's fences, so one
// tenant's vehicle crossing another tenant's boundary would raise that tenant's
// escalation chain. Fence.TenantID has existed on the struct all along and
// nothing set it or read it.
type Engine struct {
	mu     sync.RWMutex
	fences map[string]*Fence // scope(tenant, fence ID) → fence
	// state is tenant → device IMEI → fence ID → inside?. Device ids are not
	// unique across tenants, so a flat device key would let one tenant's device
	// carry another's crossing state.
	state    map[string]map[string]map[string]bool
	handlers []EventHandler
}

// scope keys a fence by its tenant. Both halves are escaped: a fence id is
// caller-supplied, so without it tenant "a" fence "b:c" and tenant "a:b" fence
// "c" would be the same entry.
func scope(tenantID, id string) string {
	return url.QueryEscape(tenantID) + ":" + url.QueryEscape(id)
}

// NewEngine creates a geofence engine.
func NewEngine() *Engine {
	return &Engine{
		fences: make(map[string]*Fence),
		state:  make(map[string]map[string]map[string]bool),
	}
}

// AddFence registers a geofence. f.TenantID names its owner and is set from the
// caller's session, never from a request body.
func (e *Engine) AddFence(f Fence) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fences[scope(f.TenantID, f.ID)] = &f
	slog.Info("geofence: added", "tenant", f.TenantID, "id", f.ID, "name", f.Name,
		"vertices", len(f.Polygon), "trigger", f.Trigger)
}

// RemoveFence removes one of the tenant's own geofences. It reports whether a
// fence was removed.
func (e *Engine) RemoveFence(tenantID, id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	k := scope(tenantID, id)
	if _, ok := e.fences[k]; !ok {
		return false
	}
	delete(e.fences, k)
	// Clean up this tenant's crossing state for the fence.
	for _, deviceState := range e.state[tenantID] {
		delete(deviceState, id)
	}
	return true
}

// ForgetTenant drops every fence and every crossing state held for a tenant.
// Implements tenancy.TenantForgetter.
func (e *Engine) ForgetTenant(tenantID string) {
	if e == nil || tenantID == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, f := range e.fences {
		if f.TenantID == tenantID {
			delete(e.fences, k)
		}
	}
	delete(e.state, tenantID)
}

// ListFences returns the tenant's own fences.
func (e *Engine) ListFences(tenantID string) []Fence {
	e.mu.RLock()
	defer e.mu.RUnlock()
	fences := make([]Fence, 0, len(e.fences))
	for _, f := range e.fences {
		if f.TenantID == tenantID {
			fences = append(fences, *f)
		}
	}
	return fences
}

// OnEvent registers a handler called when geofence events occur.
func (e *Engine) OnEvent(h EventHandler) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.handlers = append(e.handlers, h)
}

// Evaluate checks a position against the fences OF THE DEVICE'S OWN TENANT and
// emits events for transitions.
//
// tenantID is the tenant that owns the device. An empty tenant evaluates
// nothing: a fence event raises an escalation chain, and the only alternative
// to naming a tenant is raising everybody's.
func (e *Engine) Evaluate(ctx context.Context, tenantID, deviceIMEI string, lat, lon float64) []FenceEvent {
	if tenantID == "" {
		slog.Warn("geofence: refusing to evaluate a position with no tenant", "device", deviceIMEI)
		return nil
	}

	e.mu.Lock()
	// NOT deferred: the handler loop at the end of this function must run with
	// the lock RELEASED. The comment down there always said "outside the
	// critical section" and, with a defer, it never was -- a handler that
	// touched the engine would deadlock on a non-reentrant mutex, and a handler
	// that sent an SMS (which is what a fence's escalation chain does) would
	// hold every other tenant's evaluation behind its network call.
	//
	// Harmless until now only because nothing calls OnEvent -- see MESHSAT-1119,
	// the engine evaluates nothing at all -- which is exactly why it had to be
	// fixed before somebody wires the first handler up.
	if e.state[tenantID] == nil {
		e.state[tenantID] = make(map[string]map[string]bool)
	}
	if e.state[tenantID][deviceIMEI] == nil {
		e.state[tenantID][deviceIMEI] = make(map[string]bool)
	}
	deviceState := e.state[tenantID][deviceIMEI]

	var events []FenceEvent
	p := Point{Lat: lat, Lon: lon}
	now := time.Now().UTC()

	for _, fence := range e.fences {
		if !fence.Enabled || fence.TenantID != tenantID {
			continue
		}

		inside := PointInPolygon(p, fence.Polygon)
		wasInside := deviceState[fence.ID]

		if inside == wasInside {
			continue // no transition
		}

		deviceState[fence.ID] = inside

		var eventType string
		if inside && !wasInside {
			eventType = "enter"
		} else {
			eventType = "exit"
		}

		// Check trigger mode.
		if fence.Trigger == TriggerEnter && eventType != "enter" {
			continue
		}
		if fence.Trigger == TriggerExit && eventType != "exit" {
			continue
		}

		event := FenceEvent{
			TenantID:   tenantID,
			FenceID:    fence.ID,
			FenceName:  fence.Name,
			DeviceIMEI: deviceIMEI,
			EventType:  eventType,
			Lat:        lat,
			Lon:        lon,
			Timestamp:  now,
		}
		events = append(events, event)

		slog.Info("geofence: event",
			"tenant", tenantID, "fence", fence.Name, "device", deviceIMEI, "type", eventType,
			"lat", lat, "lon", lon)
	}

	// Copy the handlers, then release the lock, then call them. Both halves
	// matter: the copy is because e.handlers may be appended to by OnEvent, and
	// releasing is because a handler may do anything at all, including calling
	// back into this engine.
	handlers := make([]EventHandler, len(e.handlers))
	copy(handlers, e.handlers)
	e.mu.Unlock()

	for _, event := range events {
		for _, h := range handlers {
			h(ctx, event)
		}
	}

	return events
}

// PointInPolygon tests if a point is inside a polygon using the ray-casting algorithm.
func PointInPolygon(p Point, polygon []Point) bool {
	n := len(polygon)
	if n < 3 {
		return false
	}

	inside := false
	j := n - 1
	for i := 0; i < n; i++ {
		yi := polygon[i].Lat
		xi := polygon[i].Lon
		yj := polygon[j].Lat
		xj := polygon[j].Lon

		if ((yi > p.Lat) != (yj > p.Lat)) &&
			(p.Lon < (xj-xi)*(p.Lat-yi)/(yj-yi)+xi) {
			inside = !inside
		}
		j = i
	}
	return inside
}
