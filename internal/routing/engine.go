// Package routing implements the configurable message routing engine.
// Routes define source→destination mappings with optional keyword/device filters.
// The engine subscribes to inbound MQTT topics and dispatches messages to
// matching destination handlers.
package routing

import (
	"context"
	"encoding/json"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
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
	tenants         *tenancy.Resolver
	handlers        map[string]DestinationHandler // destination_type → handler
	mu              sync.RWMutex
	cachedRoutes    map[string]routeCache // tenant → routes
	refreshInterval time.Duration
}

type routeCache struct {
	routes    []store.Route
	refreshed time.Time
}

// NewEngine creates a new routing engine. Routes are evaluated per tenant:
// the tenant of an inbound message is the tenant that owns the publishing
// device (tenancy.Resolver), so one engine serves every tenant.
func NewEngine(s store.Store, mqtt bus.MessageBus, tenants *tenancy.Resolver) *Engine {
	if tenants == nil {
		tenants = tenancy.NewResolver(s, store.DefaultTenantID, 30*time.Second)
	}
	return &Engine{
		store:           s,
		mqtt:            mqtt,
		tenants:         tenants,
		handlers:        make(map[string]DestinationHandler),
		cachedRoutes:    make(map[string]routeCache),
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
	for _, f := range hubmqtt.DualFilters("meshsat/+/mo/decoded") {
		if err := e.mqtt.Subscribe(f, 1, e.handleMODecoded); err != nil {
			return err
		}
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

	tenantID := e.tenants.ForDeviceTopic(context.Background(), deviceID, hubmqtt.ExtractTenantID(topic))
	routes := e.getRoutes(tenantID)
	handlerCtx := tenancy.WithTenant(context.Background(), tenantID)

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
		// Sender condition (MESHSAT-964): only messages from the listed
		// origins fire the route, so kit A reaches kit B without an echo and
		// a stranger's SMS is not relayed.
		if !matchSenders(route.Senders, deviceID) {
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
		claimKey := "route:" + tenantID + ":" + msgID + ":" + route.ID
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
			"device", deviceID, "tenant", tenantID, "source", sourceType, "message", msgID)
		handler(handlerCtx, route, deviceID, json.RawMessage(payload))
	}
}

// getRoutes returns the tenant's cached routes, refreshing from DB if stale.
func (e *Engine) getRoutes(tenantID string) []store.Route {
	e.mu.RLock()
	c, ok := e.cachedRoutes[tenantID]
	e.mu.RUnlock()
	if ok && time.Since(c.refreshed) < e.refreshInterval {
		return c.routes
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	routes, err := e.store.ListRoutes(ctx, tenantID)
	if err != nil {
		slog.Error("routing: failed to load routes", "tenant", tenantID, "error", err)
		return c.routes // stale is better than nothing
	}

	e.mu.Lock()
	e.cachedRoutes[tenantID] = routeCache{routes: routes, refreshed: time.Now()}
	e.mu.Unlock()
	return routes
}

// InvalidateCache forces a route reload for every tenant on the next message.
func (e *Engine) InvalidateCache() {
	e.mu.Lock()
	e.cachedRoutes = make(map[string]routeCache)
	e.mu.Unlock()
}

// isRecipientDestination returns true for destination types where the filter
// field is a recipient address (phone number, email), not a message match condition.
func isRecipientDestination(destType string) bool {
	return destType == "sms" || destType == "email" || destType == "satellite"
}

// satelliteChannels are the message channels the "satellite" source covers.
var satelliteChannels = map[string]bool{"iridium": true, "iridium_imt": true, "globalstar": true}

// matchSource returns true if the route's source matches the message source.
// "*" matches every channel; "satellite" matches the satellite channels only,
// so the seeded "Satellite -> ..." fan-out routes stop firing on SMS and
// email traffic (MESHSAT-1022); anything else is an exact channel name.
func matchSource(routeSource, msgSource string) bool {
	if routeSource == "*" {
		return true
	}
	if strings.EqualFold(routeSource, "satellite") {
		return satelliteChannels[strings.ToLower(msgSource)]
	}
	return strings.EqualFold(routeSource, msgSource)
}

// matchSenders reports whether the message origin (device IMEI or phone
// number) is in the route's comma-separated sender list. An empty list or a
// "*" entry accepts every sender; comparison is exact after trimming.
func matchSenders(senders, origin string) bool {
	if strings.TrimSpace(senders) == "" {
		return true
	}
	for _, s := range strings.Split(senders, ",") {
		s = strings.TrimSpace(s)
		if s == "*" || (s != "" && strings.EqualFold(s, origin)) {
			return true
		}
	}
	return false
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
