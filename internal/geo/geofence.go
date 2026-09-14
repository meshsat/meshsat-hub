package geo

import (
	"context"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
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
	ID      string      `json:"id"`
	Name    string      `json:"name"`
	Polygon []Point     `json:"polygon"`  // ordered vertices (closed polygon)
	Trigger TriggerMode `json:"trigger"`  // "enter", "exit", "both"
	ChainID string      `json:"chain_id"` // escalation chain to trigger
	Enabled bool        `json:"enabled"`
	// CooldownSec suppresses repeat events for the same device and this fence
	// after one fires. 0 means the platform default. See Engine.cooldown.
	CooldownSec int    `json:"cooldown_sec,omitempty"`
	TenantID    string `json:"tenant_id,omitempty"`
}

// FenceEvent is emitted when a device crosses a geofence boundary.
type FenceEvent struct {
	// TenantID is the tenant that owns both the fence and the device. A fence
	// event raises an escalation chain, and a chain belongs to a tenant, so an
	// event that cannot say whose it is cannot be acted on.
	TenantID  string `json:"tenant_id,omitempty"`
	FenceID   string `json:"fence_id"`
	FenceName string `json:"fence_name"`
	// ChainID is the escalation chain the fence names. It is carried ON THE
	// EVENT rather than looked up again by the handler, because the handler
	// runs after Evaluate has released the lock and the fence may have been
	// edited or deleted by then -- a crossing that has already happened must
	// still page the people who were on call for it.
	//
	// This field not existing is why Fence.ChainID was inert: the form asked
	// which chain to trigger, the value was stored, and nothing that could act
	// on it ever saw it (MESHSAT-1119).
	ChainID    string    `json:"chain_id,omitempty"`
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
	store    Store
	bus      bus.MessageBus
	// cooldown is the platform default suppression window; a fence may set its
	// own with CooldownSec. lastFired is tenant -> device -> fence -> when the
	// last event for that triple was emitted.
	cooldown  time.Duration
	lastFired map[string]map[string]map[string]time.Time
	// now is time.Now, replaced in tests so a cooldown can be crossed without
	// a test that sleeps for five minutes.
	now func() time.Time
}

// SetCooldown sets the platform default suppression window (MESHSAT-1119).
//
// A geofence transition fires the moment point-in-polygon flips, so a device
// parked on a boundary -- GPS jitter is metres, and a vehicle at a depot gate is
// the obvious case -- produces enter/exit/enter/exit, and each one raises an
// escalation chain and sends a real SMS.
//
// This damps the OUTPUT: the FIRST crossing fires immediately and is never
// delayed, and further events for the same device and fence are dropped for the
// window. The alternative, dwell time, damps the input by waiting for a second
// report on the new side -- which with Iridium SBD reports minutes apart delays
// every genuine crossing by a full reporting interval. On a safety path a late
// alert is worse than a duplicate one, which is why this is the shape chosen.
func (e *Engine) SetCooldown(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cooldown = d
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
		fences:    make(map[string]*Fence),
		state:     make(map[string]map[string]map[string]bool),
		lastFired: make(map[string]map[string]map[string]time.Time),
		now:       time.Now,
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
	// Clean up this tenant's crossing state and cooldown stamps for the fence.
	for _, deviceState := range e.state[tenantID] {
		delete(deviceState, id)
	}
	for _, deviceStamps := range e.lastFired[tenantID] {
		delete(deviceStamps, id)
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
	delete(e.lastFired, tenantID)
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
// at is the POSITION's own timestamp, not the moment we processed it, and may
// be zero. It becomes the event's Timestamp, which matters for more than
// tidiness: both Hub replicas evaluate the same position, and the escalation
// handler claims a crossing once across replicas using that timestamp as the
// message's identity. A processing clock would differ between the two and page
// the on-call twice.
func (e *Engine) Evaluate(ctx context.Context, tenantID, deviceIMEI string, lat, lon float64, at time.Time) []FenceEvent {
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
	now := e.clock().UTC()
	// The event carries the position's time when it has one; the COOLDOWN is
	// always measured on our own clock, so a device with a wrong clock cannot
	// defeat it.
	eventAt := at.UTC()
	if at.IsZero() {
		eventAt = now
	}

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

		// Cooldown (MESHSAT-1119). The crossing STATE above is always updated,
		// even when the event is suppressed -- otherwise the inside/outside flag
		// goes stale and a later genuine transition is missed entirely. Only the
		// notification is dropped.
		if window := e.cooldownFor(fence); window > 0 {
			if last, ok := e.lastFiredAt(tenantID, deviceIMEI, fence.ID); ok && now.Sub(last) < window {
				slog.Info("geofence: crossing suppressed by the cooldown",
					"tenant", tenantID, "fence", fence.Name, "device", deviceIMEI,
					"type", eventType, "cooldown", window.String(),
					"since_last", now.Sub(last).Round(time.Second).String())
				continue
			}
			e.stampFired(tenantID, deviceIMEI, fence.ID, now)
		}

		event := FenceEvent{
			TenantID:   tenantID,
			ChainID:    fence.ChainID,
			FenceID:    fence.ID,
			FenceName:  fence.Name,
			DeviceIMEI: deviceIMEI,
			EventType:  eventType,
			Lat:        lat,
			Lon:        lon,
			Timestamp:  eventAt,
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

// cooldownFor is the fence's own suppression window, or the platform default.
// The caller must hold e.mu.
// clock is e.now, or time.Now when the engine was built by a zero-value literal.
func (e *Engine) clock() time.Time {
	if e.now == nil {
		return time.Now()
	}
	return e.now()
}

func (e *Engine) cooldownFor(f *Fence) time.Duration {
	if f.CooldownSec > 0 {
		return time.Duration(f.CooldownSec) * time.Second
	}
	return e.cooldown
}

// lastFiredAt and stampFired track when this device last produced an event for
// this fence. The caller must hold e.mu.
func (e *Engine) lastFiredAt(tenantID, deviceIMEI, fenceID string) (time.Time, bool) {
	t, ok := e.lastFired[tenantID][deviceIMEI][fenceID]
	return t, ok
}

func (e *Engine) stampFired(tenantID, deviceIMEI, fenceID string, at time.Time) {
	if e.lastFired[tenantID] == nil {
		e.lastFired[tenantID] = map[string]map[string]time.Time{}
	}
	if e.lastFired[tenantID][deviceIMEI] == nil {
		e.lastFired[tenantID][deviceIMEI] = map[string]time.Time{}
	}
	e.lastFired[tenantID][deviceIMEI][fenceID] = at
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
