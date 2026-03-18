package geo

import (
	"context"
	"log/slog"
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
type Engine struct {
	mu       sync.RWMutex
	fences   map[string]*Fence          // fence ID → fence
	state    map[string]map[string]bool // device IMEI → fence ID → inside?
	handlers []EventHandler
}

// NewEngine creates a geofence engine.
func NewEngine() *Engine {
	return &Engine{
		fences: make(map[string]*Fence),
		state:  make(map[string]map[string]bool),
	}
}

// AddFence registers a geofence.
func (e *Engine) AddFence(f Fence) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fences[f.ID] = &f
	slog.Info("geofence: added", "id", f.ID, "name", f.Name, "vertices", len(f.Polygon), "trigger", f.Trigger)
}

// RemoveFence removes a geofence.
func (e *Engine) RemoveFence(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.fences, id)
	// Clean up state for this fence.
	for _, deviceState := range e.state {
		delete(deviceState, id)
	}
}

// ListFences returns all configured fences.
func (e *Engine) ListFences() []Fence {
	e.mu.RLock()
	defer e.mu.RUnlock()
	fences := make([]Fence, 0, len(e.fences))
	for _, f := range e.fences {
		fences = append(fences, *f)
	}
	return fences
}

// OnEvent registers a handler called when geofence events occur.
func (e *Engine) OnEvent(h EventHandler) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.handlers = append(e.handlers, h)
}

// Evaluate checks a position against all fences and emits events for transitions.
func (e *Engine) Evaluate(ctx context.Context, deviceIMEI string, lat, lon float64) []FenceEvent {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.state[deviceIMEI] == nil {
		e.state[deviceIMEI] = make(map[string]bool)
	}

	var events []FenceEvent
	p := Point{Lat: lat, Lon: lon}
	now := time.Now().UTC()

	for _, fence := range e.fences {
		if !fence.Enabled {
			continue
		}

		inside := PointInPolygon(p, fence.Polygon)
		wasInside := e.state[deviceIMEI][fence.ID]

		if inside == wasInside {
			continue // no transition
		}

		e.state[deviceIMEI][fence.ID] = inside

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
			"fence", fence.Name, "device", deviceIMEI, "type", eventType,
			"lat", lat, "lon", lon)
	}

	// Notify handlers outside the critical section.
	for _, event := range events {
		for _, h := range e.handlers {
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
