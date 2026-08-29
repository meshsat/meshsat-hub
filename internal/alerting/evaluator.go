// Package alerting implements the configurable alerting rules engine.
// Rules are evaluated periodically and trigger escalation chains when
// conditions are met (e.g., device not seen for N hours).
package alerting

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// EscalationTrigger is the interface for triggering escalation chains.
type EscalationTrigger interface {
	Trigger(ctx context.Context, tenantID, chainID, deviceIMEI, alertType, detail string) error
}

// Evaluator periodically checks alert rules and triggers escalation chains.
type Evaluator struct {
	store     store.Store
	escalator EscalationTrigger
	interval  time.Duration

	mu    sync.Mutex
	fired map[string]time.Time // key: ruleID+deviceIMEI, value: when fired (TTL dedup)
}

// New creates a new alert rule evaluator.
func New(s store.Store, esc EscalationTrigger, interval time.Duration) *Evaluator {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	return &Evaluator{
		store:     s,
		escalator: esc,
		interval:  interval,
		fired:     make(map[string]time.Time),
	}
}

// Start runs the evaluation loop. Blocks until ctx is cancelled.
func (e *Evaluator) Start(ctx context.Context) {
	slog.Info("alerting: evaluator started", "interval", e.interval)
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("alerting: evaluator stopped")
			return
		case <-ticker.C:
			e.evaluate(ctx)
		}
	}
}

// evaluate loads all enabled rules across all tenants and checks conditions.
func (e *Evaluator) evaluate(ctx context.Context) {
	// List all rules across all tenants (empty tenantID = all).
	rules, err := e.store.ListAlertRules(ctx, "")
	if err != nil {
		slog.Error("alerting: list rules", "error", err)
		return
	}

	now := time.Now().UTC()

	// Clean expired dedup entries (older than 1 hour).
	e.mu.Lock()
	for k, t := range e.fired {
		if now.Sub(t) > time.Hour {
			delete(e.fired, k)
		}
	}
	e.mu.Unlock()

	for i := range rules {
		rule := &rules[i]
		if !rule.Enabled {
			continue
		}
		e.evaluateRule(ctx, rule, now)
	}
}

// deviceNotSeenParams holds the JSON parameters for device_not_seen condition.
type deviceNotSeenParams struct {
	ThresholdHours float64 `json:"threshold_hours"`
}

func (e *Evaluator) evaluateRule(ctx context.Context, rule *store.AlertRule, now time.Time) {
	switch rule.ConditionType {
	case "device_not_seen":
		e.evalDeviceNotSeen(ctx, rule, now)
	default:
		// Unsupported condition types are silently skipped.
		return
	}

	// Update last_evaluated timestamp.
	rule.LastEvaluated = now
	if err := e.store.UpdateAlertRule(ctx, rule.TenantID, rule); err != nil {
		slog.Error("alerting: update last_evaluated", "rule", rule.ID, "error", err)
	}
}

func (e *Evaluator) evalDeviceNotSeen(ctx context.Context, rule *store.AlertRule, now time.Time) {
	var params deviceNotSeenParams
	if err := json.Unmarshal([]byte(rule.ConditionParams), &params); err != nil {
		slog.Error("alerting: parse condition_params", "rule", rule.ID, "error", err)
		return
	}
	if params.ThresholdHours <= 0 {
		return
	}
	threshold := time.Duration(params.ThresholdHours * float64(time.Hour))

	// Get devices to check based on filter.
	devices, err := e.getFilteredDevices(ctx, rule)
	if err != nil {
		slog.Error("alerting: list devices", "rule", rule.ID, "error", err)
		return
	}

	for _, dev := range devices {
		if dev.LastSeen.IsZero() {
			continue // never seen — skip, not "not seen"
		}
		if now.Sub(dev.LastSeen) <= threshold {
			continue // seen recently enough
		}

		// Check dedup: don't fire again within 1 hour for same rule+device.
		dedupKey := rule.ID + ":" + dev.IMEI
		e.mu.Lock()
		if _, ok := e.fired[dedupKey]; ok {
			e.mu.Unlock()
			continue
		}
		e.fired[dedupKey] = now
		e.mu.Unlock()

		detail := "Device " + dev.IMEI + " not seen for " + now.Sub(dev.LastSeen).Truncate(time.Minute).String()
		if err := e.escalator.Trigger(ctx, rule.TenantID, rule.ChainID, dev.IMEI, "device_not_seen", detail); err != nil {
			slog.Error("alerting: trigger escalation",
				"rule", rule.ID, "device", dev.IMEI, "error", err)
		} else {
			slog.Warn("alerting: rule fired",
				"rule", rule.Name, "device", dev.IMEI, "detail", detail)
		}
	}
}

func (e *Evaluator) getFilteredDevices(ctx context.Context, rule *store.AlertRule) ([]store.Device, error) {
	if rule.DeviceFilter == "*" || rule.DeviceFilter == "" {
		return e.store.ListDevices(ctx, rule.TenantID)
	}
	// Try as specific IMEI first.
	dev, err := e.store.GetDevice(ctx, rule.TenantID, rule.DeviceFilter)
	if err == nil {
		return []store.Device{*dev}, nil
	}
	// Fall back to listing all devices (filter might be a group ID — not implemented yet).
	return e.store.ListDevices(ctx, rule.TenantID)
}
