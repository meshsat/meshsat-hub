// Package sos implements SOS detection on incoming MO messages.
// It subscribes to meshsat/+/mo/decoded, checks for SOS indicators
// (keywords, explicit field), publishes to the sos topic, and fires
// the escalation engine.
package sos

import (
	"context"
	"encoding/json"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
	"log/slog"
	"strings"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/escalation"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// sosKeywords are the keywords that trigger SOS detection (case-insensitive).
var sosKeywords = []string{"SOS", "MAYDAY", "EMERGENCY"}

// moDecodedMsg is the subset of the MO decoded message needed for SOS detection.
type moDecodedMsg struct {
	ID   string `json:"id"`
	IMEI string `json:"imei"`
	Text string `json:"text"`
	SOS  *bool  `json:"sos,omitempty"`
}

// SOSEvent is published to meshsat/{device_id}/sos.
type SOSEvent struct {
	IMEI    string `json:"imei"`
	Text    string `json:"text"`
	Keyword string `json:"keyword,omitempty"`
	Source  string `json:"source"` // "keyword" or "field"
}

// Detector listens for MO decoded messages and triggers SOS escalation.
type Detector struct {
	bus       bus.MessageBus
	engine    *escalation.Engine
	tenants   *tenancy.Resolver
	chainID   string // default escalation chain for SOS alerts
	dataStore store.Store
}

// NewDetector creates an SOS detector.
// chainID is the default escalation chain ID to use for SOS alerts.
// If empty, the detector will use the first available chain.
func NewDetector(b bus.MessageBus, engine *escalation.Engine, dataStore store.Store, tenants *tenancy.Resolver, chainID string) *Detector {
	if tenants == nil {
		tenants = tenancy.NewResolver(dataStore, store.DefaultTenantID, 30*time.Second)
	}
	return &Detector{
		bus:       b,
		engine:    engine,
		tenants:   tenants,
		chainID:   chainID,
		dataStore: dataStore,
	}
}

// Start subscribes to meshsat/+/mo/decoded and begins SOS detection.
func (d *Detector) Start() error {
	for _, f := range hubmqtt.DualFilters("meshsat/+/mo/decoded") {
		if err := d.bus.Subscribe(f, 1, d.handleMODecoded); err != nil {
			return err
		}
	}
	return nil
}

func (d *Detector) handleMODecoded(topic string, payload []byte) {
	var msg moDecodedMsg
	if err := json.Unmarshal(payload, &msg); err != nil {
		slog.Warn("sos: failed to unmarshal mo/decoded", "error", err)
		return
	}

	if msg.IMEI == "" {
		return
	}

	keyword, source := detectSOS(msg)
	if source == "" {
		return // not an SOS message
	}
	tenantID := d.tenants.ForDeviceTopic(context.Background(), msg.IMEI, hubmqtt.ExtractTenantID(topic))

	// One SOS event and one alert per message across all replicas.
	msgID := msg.ID
	if msgID == "" {
		msgID = hubmqtt.FallbackMessageID(topic, payload)
	}
	if d.dataStore != nil {
		claimCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		won, err := d.dataStore.ClaimOnce(claimCtx, "sos:"+tenantID+":"+msgID)
		cancel()
		if err != nil {
			slog.Error("sos: claim failed, not triggering", "imei", msg.IMEI, "message", msgID, "error", err)
			return
		}
		if !won {
			slog.Debug("sos: already handled by another replica", "imei", msg.IMEI, "message", msgID)
			return
		}
	}

	slog.Warn("sos: SOS detected",
		"imei", msg.IMEI, "keyword", keyword, "source", source, "text", msg.Text)

	// Publish SOS event to MQTT.
	event := SOSEvent{
		IMEI:    msg.IMEI,
		Text:    msg.Text,
		Keyword: keyword,
		Source:  source,
	}
	if err := d.bus.PublishJSON(hubmqtt.TopicSOSFor(tenantID, msg.IMEI), 1, false, event); err != nil {
		slog.Error("sos: failed to publish SOS event", "error", err, "imei", msg.IMEI)
	}

	// Trigger escalation (if engine is configured).
	if d.engine == nil {
		return
	}
	chainID := d.resolveChainID(tenantID)
	if chainID == "" {
		slog.Warn("sos: no escalation chain configured, alert logged only", "imei", msg.IMEI)
		return
	}

	alert := &store.Alert{
		ChainID:    chainID,
		DeviceIMEI: msg.IMEI,
		Type:       "sos",
		Detail:     msg.Text,
	}
	if err := d.engine.Trigger(context.Background(), tenantID, alert); err != nil {
		slog.Error("sos: failed to trigger escalation", "error", err, "imei", msg.IMEI)
	}
}

// detectSOS checks if a decoded MO message contains SOS indicators.
// Returns the matched keyword and source ("keyword" or "field"), or empty strings if no SOS.
func detectSOS(msg moDecodedMsg) (keyword, source string) {
	// Check explicit sos field first.
	if msg.SOS != nil && *msg.SOS {
		return "", "field"
	}

	// Check for SOS keywords in text (case-insensitive).
	upper := strings.ToUpper(msg.Text)
	for _, kw := range sosKeywords {
		if strings.Contains(upper, kw) {
			return kw, "keyword"
		}
	}

	return "", ""
}

// resolveChainID returns the escalation chain ID to use. If a specific chain
// is configured, use it. Otherwise, try the first available chain.
func (d *Detector) resolveChainID(tenantID string) string {
	if d.chainID != "" {
		return d.chainID
	}

	if d.dataStore == nil {
		return ""
	}

	// Fall back to the first available escalation chain.
	chains, err := d.dataStore.ListEscalationChains(context.Background(), tenantID)
	if err != nil || len(chains) == 0 {
		return ""
	}
	return chains[0].ID
}
