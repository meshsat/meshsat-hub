package reticulum

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"time"
)

// RouteHintPublisherConfig controls routing hint broadcast behavior.
type RouteHintPublisherConfig struct {
	// Interval is how often routing hints are published. Default: 60s.
	Interval time.Duration

	// Topic is the MQTT topic to publish hints to.
	Topic string
}

// DefaultRouteHintPublisherConfig returns sensible defaults.
func DefaultRouteHintPublisherConfig() RouteHintPublisherConfig {
	return RouteHintPublisherConfig{
		Interval: 60 * time.Second,
		Topic:    ReticulumRoutesTopic,
	}
}

// RouteHint is a single routing hint published to bridges.
type RouteHint struct {
	DestHash  string  `json:"dest_hash"`
	Interface string  `json:"interface"`
	Cost      float64 `json:"cost"`
	Hops      int     `json:"hops"`
	ExpiresAt string  `json:"expires_at"`
}

// RouteHintMessage is the JSON payload published to the routes MQTT topic.
type RouteHintMessage struct {
	HubDestHash string      `json:"hub_dest_hash"`
	Timestamp   string      `json:"timestamp"`
	Routes      []RouteHint `json:"routes"`
}

// RouteHintPublisher periodically publishes the Hub's routing table as hints
// to bridges via MQTT. This allows bridges to pre-populate their routing
// tables and learn about reachable destinations without waiting for announces
// or path discovery floods.
type RouteHintPublisher struct {
	router *Router
	mqtt   MQTTPublisher
	hubID  *HubIdentity
	config RouteHintPublisherConfig

	published atomic.Int64
}

// NewRouteHintPublisher creates a new routing hint publisher.
func NewRouteHintPublisher(router *Router, mqtt MQTTPublisher, hubID *HubIdentity, cfg RouteHintPublisherConfig) *RouteHintPublisher {
	return &RouteHintPublisher{
		router: router,
		mqtt:   mqtt,
		hubID:  hubID,
		config: cfg,
	}
}

// Run starts the periodic hint publisher. Blocks until context is cancelled.
func (p *RouteHintPublisher) Run(ctx context.Context) {
	ticker := time.NewTicker(p.config.Interval)
	defer ticker.Stop()

	slog.Info("reticulum: route hint publisher started",
		"interval", p.config.Interval,
		"topic", p.config.Topic,
	)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.publish()
		}
	}
}

// publish sends the current routing table as hints to MQTT.
func (p *RouteHintPublisher) publish() {
	if p.mqtt == nil || !p.mqtt.IsConnected() {
		return
	}

	routes := p.router.AllRoutes()
	if len(routes) == 0 {
		return
	}

	hints := make([]RouteHint, len(routes))
	for i, r := range routes {
		hints[i] = RouteHint{
			DestHash:  r.DestHash,
			Interface: r.Interface,
			Cost:      r.Cost,
			Hops:      r.Hops,
			ExpiresAt: r.ExpiresAt,
		}
	}

	hubDest := ""
	if p.hubID != nil && p.hubID.IsLoaded() {
		hubDest = p.hubID.DestHashHex()
	}

	msg := RouteHintMessage{
		HubDestHash: hubDest,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Routes:      hints,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		slog.Error("reticulum: route hint marshal failed", "error", err)
		return
	}

	if err := p.mqtt.Publish(p.config.Topic, 1, true, data); err != nil {
		slog.Error("reticulum: route hint publish failed", "error", err)
		return
	}

	p.published.Add(1)
	slog.Debug("reticulum: routing hints published",
		"routes", len(hints),
		"topic", p.config.Topic,
	)
}

// PublishNow triggers an immediate hint broadcast (e.g. after learning a new route).
func (p *RouteHintPublisher) PublishNow() {
	p.publish()
}

// PublishedCount returns how many hint messages have been published.
func (p *RouteHintPublisher) PublishedCount() int64 {
	return p.published.Load()
}
