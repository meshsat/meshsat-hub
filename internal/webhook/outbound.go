package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	hubmqtt "github.com/meshsat/meshsat-hub/internal/mqtt"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// EventType classifies the event that triggered the webhook.
type EventType string

const (
	EventMO        EventType = "mo"        // Mobile Originated message received
	EventSOS       EventType = "sos"       // SOS event
	EventPosition  EventType = "position"  // Position update
	EventTelemetry EventType = "telemetry" // Telemetry data
	EventMTStatus  EventType = "mt_status" // MT delivery status change
)

// WebhookConfig defines a single outbound webhook target.
type WebhookConfig struct {
	ID string `json:"id"`
	// TenantID is the tenant that owns this webhook and the ONLY tenant whose
	// events it may receive (MESHSAT-1118). It is set from the caller's session
	// in CreateWebhook and is never read from a request body -- see the
	// overwrite in handler.go and the test that pins it.
	TenantID   string      `json:"tenant_id,omitempty"`
	URL        string      `json:"url"`
	Secret     string      `json:"secret,omitempty"` // HMAC-SHA256 signing secret
	Events     []EventType `json:"events"`           // which events to fire for
	MaxRetries int         `json:"max_retries"`      // default 3
	TimeoutSec int         `json:"timeout_sec"`      // default 10
	Enabled    bool        `json:"enabled"`
}

// WebhookPayload is the JSON body sent to the webhook URL.
type WebhookPayload struct {
	ID        string          `json:"id"`
	Event     EventType       `json:"event"`
	DeviceID  string          `json:"device_id"`
	Timestamp string          `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

// DeliveryLog records a webhook delivery attempt.
type DeliveryLog struct {
	// TenantID scopes the log to the tenant whose webhook was delivered to.
	// It stays off the wire: RecentLogs already filters to the caller's own
	// tenant, so the field would only ever echo what the caller asked for.
	TenantID   string `json:"-"`
	WebhookID  string `json:"webhook_id"`
	Event      string `json:"event"`
	DeviceID   string `json:"device_id"`
	StatusCode int    `json:"status_code"`
	Error      string `json:"error,omitempty"`
	Timestamp  string `json:"timestamp"`
	Attempt    int    `json:"attempt"`
	// LatencyMS is how long the attempt took. The Delivery Logs table has had a
	// Latency column since it was written and nothing ever measured one, so
	// every row rendered "undefinedms".
	LatencyMS int64 `json:"latency_ms"`
}

// ReloadTopic tells every replica that a tenant's webhooks changed.
//
// The dispatcher holds its targets in memory because Fire runs per message, but
// the API writes them to the database, and there are two Hub replicas. Without
// this, a webhook created on one replica fires for that replica's share of the
// traffic and not the other's -- which on a two-replica deployment looks like a
// flaky customer endpoint rather than a bug. Same shape as tenancy.StatusTopic
// and tenancy.PurgeTopic.
const ReloadTopic = "meshsat/hub/webhooks/changed"

type reloadEvent struct {
	TenantID string `json:"tenant_id"`
}

// Store is the slice of the persistence layer this package needs. It is
// declared here, one narrow interface, rather than taking store.Store: the
// dispatcher is reachable from internal/routing, which carries field traffic.
type Store interface {
	ListTenants(ctx context.Context) ([]store.Tenant, error)
	ListWebhooks(ctx context.Context, tenantID string) ([]store.WebhookConfig, error)
	SaveWebhook(ctx context.Context, tenantID string, w *store.WebhookConfig) error
	DeleteWebhook(ctx context.Context, tenantID, id string) error
}

// Dispatcher manages outbound webhook delivery.
type Dispatcher struct {
	mu       sync.RWMutex
	webhooks []WebhookConfig
	client   *http.Client
	mqtt     bus.MessageBus
	store    Store
	logs     []DeliveryLog
	logMu    sync.Mutex

	// allowLoopback lifts the outbound target check so the dispatcher's own
	// tests can deliver to an httptest server on 127.0.0.1.
	//
	// It is set by AllowLoopbackTargetsForTest and by nothing else: there is
	// deliberately no config key, no env var and no API for it, because a
	// request-forgery guard with a switch on the outside is not a guard. If you
	// are reading this because you want to turn it on in production, the answer
	// is no -- point the webhook at a public address.
	allowLoopback bool
}

// AllowLoopbackTargetsForTest permits delivery to loopback and private
// addresses. Tests only. See the field comment.
func (d *Dispatcher) AllowLoopbackTargetsForTest() { d.allowLoopback = true }

// checkTarget applies the outbound target policy unless a test has lifted it.
func (d *Dispatcher) checkTarget(raw string) error {
	if d.allowLoopback {
		return nil
	}
	return ValidateTarget(raw)
}

// NewDispatcher creates a new webhook dispatcher.
func NewDispatcher(mqtt bus.MessageBus) *Dispatcher {
	return &Dispatcher{
		client: &http.Client{Timeout: 10 * time.Second},
		mqtt:   mqtt,
	}
}

// SetWebhooks replaces all webhook configurations.
func (d *Dispatcher) SetWebhooks(configs []WebhookConfig) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.webhooks = configs
}

// AddWebhook appends a webhook configuration.
func (d *Dispatcher) AddWebhook(cfg WebhookConfig) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if cfg.TimeoutSec <= 0 {
		cfg.TimeoutSec = 10
	}
	d.webhooks = append(d.webhooks, cfg)
}

// RemoveWebhook removes one of the tenant's webhooks by ID. It reports whether
// a webhook was removed, so the handler can answer 404 rather than pretending a
// delete of somebody else's webhook succeeded.
func (d *Dispatcher) RemoveWebhook(tenantID, id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, w := range d.webhooks {
		if w.ID == id && w.TenantID == tenantID {
			d.webhooks = append(d.webhooks[:i], d.webhooks[i+1:]...)
			return true
		}
	}
	return false
}

// ForgetTenant drops every webhook and delivery log held for a tenant.
// Implements tenancy.TenantForgetter, so a purged account stops being a live
// outbound target on every replica rather than lingering until a restart.
func (d *Dispatcher) ForgetTenant(tenantID string) {
	if d == nil || tenantID == "" {
		return
	}
	d.mu.Lock()
	kept := d.webhooks[:0]
	for _, w := range d.webhooks {
		if w.TenantID != tenantID {
			kept = append(kept, w)
		}
	}
	d.webhooks = kept
	d.mu.Unlock()

	d.logMu.Lock()
	keptLogs := d.logs[:0]
	for _, l := range d.logs {
		if l.TenantID != tenantID {
			keptLogs = append(keptLogs, l)
		}
	}
	d.logs = keptLogs
	d.logMu.Unlock()
}

// ListWebhooksRaw returns EVERY tenant's webhook configs as raw JSON, secrets
// redacted. Implements backup.WebhookLister, which is platform state behind
// RequirePlatformAdmin (MESHSAT-1116) -- this is the one cross-tenant view and
// it is deliberate. Nothing tenant-facing may call it.
func (d *Dispatcher) ListWebhooksRaw() json.RawMessage {
	d.mu.RLock()
	out := make([]WebhookConfig, len(d.webhooks))
	for i, w := range d.webhooks {
		out[i] = w
		if out[i].Secret != "" {
			out[i].Secret = "****"
		}
	}
	d.mu.RUnlock()
	data, _ := json.Marshal(out)
	return json.RawMessage(data)
}

// ListWebhooks returns the tenant's own webhook configs, secrets redacted.
func (d *Dispatcher) ListWebhooks(tenantID string) []WebhookConfig {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]WebhookConfig, 0, len(d.webhooks))
	for _, w := range d.webhooks {
		if w.TenantID != tenantID {
			continue
		}
		if w.Secret != "" {
			w.Secret = "****"
		}
		out = append(out, w)
	}
	return out
}

// RecentLogs returns the tenant's most recent delivery logs (up to limit).
func (d *Dispatcher) RecentLogs(tenantID string, limit int) []DeliveryLog {
	d.logMu.Lock()
	defer d.logMu.Unlock()
	if limit <= 0 {
		limit = len(d.logs)
	}
	out := make([]DeliveryLog, 0, limit)
	// Newest first, then reversed, so a busy tenant's own entries are not
	// crowded out of the window by another tenant's traffic -- which is what a
	// "last N rows then filter" would do.
	for i := len(d.logs) - 1; i >= 0 && len(out) < limit; i-- {
		if d.logs[i].TenantID == tenantID {
			out = append(out, d.logs[i])
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Fire sends an event to the matching webhooks OF ONE TENANT.
//
// MESHSAT-1118: this used to select targets by event type alone, with no tenant
// on the config at all, so every registered webhook received every tenant's
// message contents, positions and SOS events. Both call sites now supply the
// tenant -- the MQTT subscriber parses it out of the topic, the routing engine
// takes it from the context it already sets.
//
// An empty tenantID matches nothing. That is deliberate: a caller that cannot
// say whose event this is must not be given a fan-out to everyone.
func (d *Dispatcher) Fire(tenantID string, event EventType, deviceID string, data json.RawMessage) {
	if tenantID == "" {
		slog.Warn("webhook: refusing to fire an event with no tenant", "event", event, "device", deviceID)
		return
	}
	d.mu.RLock()
	targets := make([]WebhookConfig, 0)
	for _, w := range d.webhooks {
		if !w.Enabled || w.TenantID != tenantID {
			continue
		}
		for _, e := range w.Events {
			if e == event {
				targets = append(targets, w)
				break
			}
		}
	}
	d.mu.RUnlock()

	if len(targets) == 0 {
		return
	}

	payload := WebhookPayload{
		ID:        fmt.Sprintf("wh-%d", time.Now().UnixNano()),
		Event:     event,
		DeviceID:  deviceID,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data:      data,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("webhook: marshal payload", "error", err)
		return
	}

	for _, target := range targets {
		go d.deliver(target, body, payload.ID, event, deviceID)
	}
}

// deliver posts one payload to one target, retrying with backoff.
//
// event and deviceID travel alongside the encoded body only so the delivery log
// can record them. They used to be dropped: recordLog was called with the
// payload ID in the event column and an empty device, so every row in the
// customer-facing log read "wh-1757…" with no device against it.
func (d *Dispatcher) deliver(target WebhookConfig, body []byte, payloadID string, event EventType, deviceID string) {
	timeout := time.Duration(target.TimeoutSec) * time.Second
	client := &http.Client{Timeout: timeout}

	wait := 1 * time.Second
	// One template for every entry this delivery writes, so the tenant, the
	// event and the device cannot be filled in on some paths and forgotten on
	// others -- which is what happened to the device for every row.
	entry := func(attempt int, status int, errMsg string, took time.Duration) DeliveryLog {
		return DeliveryLog{
			TenantID: target.TenantID, WebhookID: target.ID, Event: string(event),
			DeviceID: deviceID, StatusCode: status, Error: errMsg,
			Attempt: attempt, LatencyMS: took.Milliseconds(),
		}
	}

	// Re-checked here and not only at registration: a hostname that resolved
	// publicly when the webhook was created can resolve to a cluster address by
	// the time it is delivered to, and only this check sees that.
	if err := d.checkTarget(target.URL); err != nil {
		slog.Warn("webhook: refusing to deliver to an unsafe target", "id", target.ID, "error", err)
		d.recordLog(entry(0, 0, err.Error(), 0))
		return
	}

	for attempt := 0; attempt <= target.MaxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(wait)
			wait *= 2
			if wait > 30*time.Second {
				wait = 30 * time.Second
			}
		}

		req, err := http.NewRequest("POST", target.URL, bytes.NewReader(body))
		if err != nil {
			d.recordLog(entry(attempt, 0, err.Error(), 0))
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-MeshSat-Event", payloadID)

		// HMAC-SHA256 signature
		if target.Secret != "" {
			mac := hmac.New(sha256.New, []byte(target.Secret))
			mac.Write(body)
			sig := hex.EncodeToString(mac.Sum(nil))
			req.Header.Set("X-Hub-Signature-256", "sha256="+sig)
		}

		started := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			slog.Warn("webhook: delivery failed", "url", target.URL, "error", err, "attempt", attempt)
			d.recordLog(entry(attempt, 0, err.Error(), time.Since(started)))
			continue
		}
		_ = resp.Body.Close()

		d.recordLog(entry(attempt, resp.StatusCode, "", time.Since(started)))

		if resp.StatusCode < 400 {
			slog.Debug("webhook: delivered", "url", target.URL, "status", resp.StatusCode)
			return
		}

		slog.Warn("webhook: delivery returned error", "url", target.URL, "status", resp.StatusCode, "attempt", attempt)
	}
}

func (d *Dispatcher) recordLog(log DeliveryLog) {
	log.Timestamp = time.Now().UTC().Format(time.RFC3339)
	d.logMu.Lock()
	d.logs = append(d.logs, log)
	// Keep last 1000 entries
	if len(d.logs) > 1000 {
		d.logs = d.logs[len(d.logs)-1000:]
	}
	d.logMu.Unlock()
}

// Start subscribes to MQTT topics and fires webhooks for matching events.
func (d *Dispatcher) Start(mqtt bus.MessageBus) error {
	subs := []struct {
		topic string
		event EventType
	}{
		{"meshsat/+/mo/decoded", EventMO},
		{"meshsat/+/sos", EventSOS},
		{"meshsat/+/position", EventPosition},
		{"meshsat/+/telemetry", EventTelemetry},
		{"meshsat/+/mt/status", EventMTStatus},
	}

	for _, sub := range subs {
		evt := sub.event
		// DualFilters, not the legacy filter alone. "meshsat/+/mo/decoded" has
		// four segments and matches only the DEFAULT tenant's topics, so before
		// this every non-default tenant's events reached no webhook at all --
		// while the platform tenant's reached all of them (MESHSAT-1118).
		for _, filter := range hubmqtt.DualFilters(sub.topic) {
			f := filter
			if err := mqtt.Subscribe(f, 1, func(topic string, payload []byte) {
				tenantID, deviceID, _, ok := hubmqtt.ParseDeviceTopic(topic)
				if !ok {
					return
				}
				d.Fire(tenantID, evt, deviceID, json.RawMessage(payload))
			}); err != nil {
				return fmt.Errorf("webhook subscribe %s: %w", f, err)
			}
		}
	}

	// A change made on another replica.
	if err := mqtt.Subscribe(ReloadTopic, 1, func(_ string, payload []byte) {
		var ev reloadEvent
		if err := json.Unmarshal(payload, &ev); err != nil || ev.TenantID == "" {
			return
		}
		if err := d.ReloadTenant(context.Background(), ev.TenantID); err != nil {
			slog.Error("webhook: reload after a change on another replica", "tenant", ev.TenantID, "error", err)
		}
	}); err != nil {
		return fmt.Errorf("webhook subscribe %s: %w", ReloadTopic, err)
	}

	slog.Info("webhook: dispatcher started", "webhooks", len(d.webhooks))
	return nil
}
