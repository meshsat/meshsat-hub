// Package routing implements the configurable message routing engine.
// Routes define source→destination mappings with optional keyword/device filters.
// The engine subscribes to inbound MQTT topics and dispatches messages to
// matching destination handlers.
package routing

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// DestinationHandler processes a routed message for a specific destination type.
type DestinationHandler func(ctx context.Context, route *store.Route, deviceID string, payload json.RawMessage)

// Engine evaluates routing rules and dispatches messages to destination handlers.
type Engine struct {
	store           store.Store
	mqtt            bus.MessageBus
	tenantID        string
	handlers        map[string]DestinationHandler // destination_type → handler
	mu              sync.RWMutex
	cachedRoutes    []store.Route
	lastRefresh     time.Time
	refreshInterval time.Duration
}

// NewEngine creates a new routing engine.
func NewEngine(s store.Store, mqtt bus.MessageBus, tenantID string) *Engine {
	return &Engine{
		store:           s,
		mqtt:            mqtt,
		tenantID:        tenantID,
		handlers:        make(map[string]DestinationHandler),
		refreshInterval: 30 * time.Second,
	}
}

// RegisterHandler registers a destination handler for a destination type.
func (e *Engine) RegisterHandler(destType string, handler DestinationHandler) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.handlers[destType] = handler
	slog.Info("routing: handler registered", "destination", destType)
}

// Start subscribes to inbound MQTT topics and begins route evaluation.
func (e *Engine) Start() error {
	// Subscribe to mo/decoded (main message flow) for routing.
	if err := e.mqtt.Subscribe("meshsat/+/mo/decoded", 1, e.handleMODecoded); err != nil {
		return err
	}
	slog.Info("routing: engine started")
	return nil
}

func (e *Engine) handleMODecoded(topic string, payload []byte) {
	deviceID := hubmqtt.ExtractDeviceID(topic)
	if deviceID == "" {
		return
	}

	// Extract source channel from message.
	var msg struct {
		ID      string `json:"id"`
		Channel string `json:"channel"`
		Text    string `json:"text"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil {
		return
	}
	// Stable message identity for the dispatch claim: the publisher's id, else
	// a hash of topic+payload that every replica derives identically.
	msgID := msg.ID
	if msgID == "" {
		msgID = hubmqtt.FallbackMessageID(topic, payload)
	}

	sourceType := msg.Channel
	if sourceType == "" {
		sourceType = "*"
	}

	routes := e.getRoutes()

	e.mu.RLock()
	handlers := e.handlers
	e.mu.RUnlock()

	for i := range routes {
		route := &routes[i]
		if !route.Enabled {
			continue
		}
		if !matchSource(route.SourceType, sourceType) {
			continue
		}
		// For sms/email destinations, the filter IS the recipient address —
		// not a message match condition. Skip matchFilter for these. [MESHSAT-448]
		if !isRecipientDestination(route.DestinationType) {
			if !matchFilter(route.Filter, deviceID, msg.Text) {
				continue
			}
		}

		handler, ok := handlers[route.DestinationType]
		if !ok {
			continue
		}

		// Exactly one replica dispatches a given (message, route) pair: the
		// claim is a primary-key insert, so a second replica (or a redelivery)
		// gets false. On a store error we skip rather than risk a double send.
		claimKey := "route:" + e.tenantID + ":" + msgID + ":" + route.ID
		claimCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		won, err := e.store.ClaimOnce(claimCtx, claimKey)
		cancel()
		if err != nil {
			slog.Error("routing: claim failed, not dispatching", "route", route.Name, "message", msgID, "error", err)
			continue
		}
		if !won {
			slog.Debug("routing: already dispatched by another replica", "route", route.Name, "message", msgID)
			continue
		}

		slog.Info("routing: route matched", "route", route.Name, "dest", route.DestinationType,
			"device", deviceID, "source", sourceType, "message", msgID)
		handler(context.Background(), route, deviceID, json.RawMessage(payload))
	}
}

// getRoutes returns cached routes, refreshing from DB if stale.
func (e *Engine) getRoutes() []store.Route {
	e.mu.RLock()
	if time.Since(e.lastRefresh) < e.refreshInterval && e.cachedRoutes != nil {
		routes := e.cachedRoutes
		e.mu.RUnlock()
		return routes
	}
	e.mu.RUnlock()

	// Refresh from store.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	routes, err := e.store.ListRoutes(ctx, e.tenantID)
	if err != nil {
		slog.Warn("routing: failed to refresh routes", "error", err)
		e.mu.RLock()
		routes = e.cachedRoutes // use stale cache
		e.mu.RUnlock()
		return routes
	}

	e.mu.Lock()
	e.cachedRoutes = routes
	e.lastRefresh = time.Now()
	e.mu.Unlock()

	return routes
}

// InvalidateCache forces a refresh on the next message.
func (e *Engine) InvalidateCache() {
	e.mu.Lock()
	e.lastRefresh = time.Time{}
	e.mu.Unlock()
}

// isRecipientDestination returns true for destination types where the filter
// field is a recipient address (phone number, email), not a message match condition.
func isRecipientDestination(destType string) bool {
	return destType == "sms" || destType == "email"
}

// matchSource returns true if the route's source matches the message source.
func matchSource(routeSource, msgSource string) bool {
	if routeSource == "*" {
		return true
	}
	return strings.EqualFold(routeSource, msgSource)
}

// matchFilter returns true if the message matches the route's filter.
// Empty filter matches everything. Filter can be a device IMEI or keyword.
func matchFilter(filter, deviceID, text string) bool {
	if filter == "" {
		return true
	}
	// Check if filter matches device IMEI.
	if filter == deviceID {
		return true
	}
	// Check if filter is a keyword present in the text.
	return strings.Contains(strings.ToUpper(text), strings.ToUpper(filter))
}
