package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	hubmqtt "github.com/cubeos-app/meshsat-hub/internal/mqtt"
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
	ID         string      `json:"id"`
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
	WebhookID  string `json:"webhook_id"`
	Event      string `json:"event"`
	DeviceID   string `json:"device_id"`
	StatusCode int    `json:"status_code"`
	Error      string `json:"error,omitempty"`
	Timestamp  string `json:"timestamp"`
	Attempt    int    `json:"attempt"`
}

// Dispatcher manages outbound webhook delivery.
type Dispatcher struct {
	mu       sync.RWMutex
	webhooks []WebhookConfig
	client   *http.Client
	mqtt     *hubmqtt.Client
	logs     []DeliveryLog
	logMu    sync.Mutex
}

// NewDispatcher creates a new webhook dispatcher.
func NewDispatcher(mqtt *hubmqtt.Client) *Dispatcher {
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

// RemoveWebhook removes a webhook by ID.
func (d *Dispatcher) RemoveWebhook(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, w := range d.webhooks {
		if w.ID == id {
			d.webhooks = append(d.webhooks[:i], d.webhooks[i+1:]...)
			return
		}
	}
}

// ListWebhooksRaw returns all webhook configs as raw JSON (secrets redacted).
// Implements backup.WebhookLister.
func (d *Dispatcher) ListWebhooksRaw() json.RawMessage {
	data, _ := json.Marshal(d.ListWebhooks())
	return json.RawMessage(data)
}

// ListWebhooks returns all webhook configs with secrets redacted.
func (d *Dispatcher) ListWebhooks() []WebhookConfig {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]WebhookConfig, len(d.webhooks))
	for i, w := range d.webhooks {
		out[i] = w
		if out[i].Secret != "" {
			out[i].Secret = "****"
		}
	}
	return out
}

// RecentLogs returns the most recent delivery logs (up to limit).
func (d *Dispatcher) RecentLogs(limit int) []DeliveryLog {
	d.logMu.Lock()
	defer d.logMu.Unlock()
	if limit <= 0 || limit > len(d.logs) {
		limit = len(d.logs)
	}
	start := len(d.logs) - limit
	out := make([]DeliveryLog, limit)
	copy(out, d.logs[start:])
	return out
}

// Fire sends a webhook event to all matching webhook targets.
func (d *Dispatcher) Fire(event EventType, deviceID string, data json.RawMessage) {
	d.mu.RLock()
	targets := make([]WebhookConfig, 0)
	for _, w := range d.webhooks {
		if !w.Enabled {
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
		go d.deliver(target, body, payload.ID)
	}
}

func (d *Dispatcher) deliver(target WebhookConfig, body []byte, payloadID string) {
	timeout := time.Duration(target.TimeoutSec) * time.Second
	client := &http.Client{Timeout: timeout}

	wait := 1 * time.Second
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
			d.recordLog(target.ID, payloadID, "", 0, err.Error(), attempt)
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

		resp, err := client.Do(req)
		if err != nil {
			slog.Warn("webhook: delivery failed", "url", target.URL, "error", err, "attempt", attempt)
			d.recordLog(target.ID, payloadID, "", 0, err.Error(), attempt)
			continue
		}
		_ = resp.Body.Close()

		d.recordLog(target.ID, payloadID, "", resp.StatusCode, "", attempt)

		if resp.StatusCode < 400 {
			slog.Debug("webhook: delivered", "url", target.URL, "status", resp.StatusCode)
			return
		}

		slog.Warn("webhook: delivery returned error", "url", target.URL, "status", resp.StatusCode, "attempt", attempt)
	}
}

func (d *Dispatcher) recordLog(webhookID, event, deviceID string, status int, errMsg string, attempt int) {
	log := DeliveryLog{
		WebhookID:  webhookID,
		Event:      event,
		DeviceID:   deviceID,
		StatusCode: status,
		Error:      errMsg,
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		Attempt:    attempt,
	}
	d.logMu.Lock()
	d.logs = append(d.logs, log)
	// Keep last 1000 entries
	if len(d.logs) > 1000 {
		d.logs = d.logs[len(d.logs)-1000:]
	}
	d.logMu.Unlock()
}

// Start subscribes to MQTT topics and fires webhooks for matching events.
func (d *Dispatcher) Start(mqtt *hubmqtt.Client) error {
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
		if err := mqtt.Subscribe(sub.topic, 1, func(topic string, payload []byte) {
			deviceID := hubmqtt.ExtractDeviceID(topic)
			d.Fire(evt, deviceID, json.RawMessage(payload))
		}); err != nil {
			return fmt.Errorf("webhook subscribe %s: %w", sub.topic, err)
		}
	}

	slog.Info("webhook: dispatcher started", "webhooks", len(d.webhooks))
	return nil
}
