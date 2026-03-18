// Package escalation implements the SOS/alert escalation chain engine.
// Alerts progress through ordered notification tiers until acknowledged
// or all tiers are exhausted. Each tier has configurable wait time and
// retry count with exponential backoff.
package escalation

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/apprise"
	"github.com/cubeos-app/meshsat-hub/internal/store"
)

// Notifier sends a notification to a set of targets.
// Implementations: Apprise (MESHSAT-112), ntfy (MESHSAT-113).
type Notifier interface {
	// Notify sends a message to the given targets. Returns nil on success.
	Notify(ctx context.Context, targets []string, subject, body string) error
}

// LogNotifier is a no-op notifier that logs notifications (used until
// Apprise/ntfy are integrated).
type LogNotifier struct{}

func (LogNotifier) Notify(_ context.Context, targets []string, subject, body string) error {
	slog.Info("escalation: notify (no backend configured)",
		"targets", targets, "subject", subject, "body_len", len(body))
	return nil
}

// Engine manages the escalation lifecycle for active alerts.
type Engine struct {
	store    store.Store
	notifier Notifier
	interval time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
}

// New creates an escalation engine.
func New(s store.Store, n Notifier) *Engine {
	if n == nil {
		n = LogNotifier{}
	}
	return &Engine{
		store:    s,
		notifier: n,
		interval: 10 * time.Second,
	}
}

// SetNotifier replaces the notification backend (e.g., when Apprise starts).
func (e *Engine) SetNotifier(n Notifier) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.notifier = n
}

// Start begins the escalation processing loop. Blocks until ctx is cancelled.
func (e *Engine) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	e.mu.Lock()
	e.cancel = cancel
	e.mu.Unlock()

	slog.Info("escalation: engine started", "interval", e.interval)
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("escalation: engine stopped")
			return
		case <-ticker.C:
			e.processAlerts(ctx)
		}
	}
}

// Trigger creates a new alert and starts escalation.
func (e *Engine) Trigger(ctx context.Context, tenantID string, alert *store.Alert) error {
	if alert.ID == "" {
		alert.ID = fmt.Sprintf("alert-%d", time.Now().UnixNano())
	}
	alert.State = store.AlertStateTriggered
	alert.CurrentTier = 0
	alert.Retries = 0
	now := time.Now().UTC()
	alert.CreatedAt = now
	alert.UpdatedAt = now
	alert.NextEscAt = now // process immediately on next tick

	if err := e.store.CreateAlert(ctx, tenantID, alert); err != nil {
		return fmt.Errorf("escalation: create alert: %w", err)
	}

	slog.Warn("escalation: alert triggered",
		"id", alert.ID, "type", alert.Type, "device", alert.DeviceIMEI, "chain", alert.ChainID)
	return nil
}

// Acknowledge stops escalation for an alert.
func (e *Engine) Acknowledge(ctx context.Context, tenantID, alertID, ackedBy string) error {
	alert, err := e.store.GetAlert(ctx, tenantID, alertID)
	if err != nil {
		return fmt.Errorf("escalation: get alert: %w", err)
	}
	if alert.State == store.AlertStateAcknowledged {
		return nil // already acked
	}
	alert.State = store.AlertStateAcknowledged
	alert.AckedBy = ackedBy
	alert.AckedAt = time.Now().UTC()
	alert.UpdatedAt = alert.AckedAt
	if err := e.store.UpdateAlert(ctx, tenantID, alert); err != nil {
		return fmt.Errorf("escalation: update alert: %w", err)
	}
	slog.Info("escalation: alert acknowledged", "id", alertID, "by", ackedBy)
	return nil
}

// processAlerts checks all active alerts and escalates as needed.
func (e *Engine) processAlerts(ctx context.Context) {
	// Process all tenants by listing active alerts globally.
	// In practice, we'd need a tenant-aware query. For now, use default tenant.
	// TODO: support multi-tenant alert processing
	alerts, err := e.store.ListAlerts(ctx, "", true, 100)
	if err != nil {
		slog.Error("escalation: list active alerts", "error", err)
		return
	}

	now := time.Now().UTC()
	for i := range alerts {
		alert := &alerts[i]
		if now.Before(alert.NextEscAt) {
			continue // not time to escalate yet
		}
		e.processAlert(ctx, alert, now)
	}
}

func (e *Engine) processAlert(ctx context.Context, alert *store.Alert, now time.Time) {
	// Load the chain to get tier configuration.
	chain, err := e.store.GetEscalationChain(ctx, "", alert.ChainID)
	if err != nil {
		slog.Error("escalation: chain not found", "chain_id", alert.ChainID, "error", err)
		return
	}

	if alert.CurrentTier >= len(chain.Tiers) {
		// All tiers exhausted.
		alert.State = store.AlertStateExhausted
		alert.UpdatedAt = now
		_ = e.store.UpdateAlert(ctx, "", alert)
		slog.Warn("escalation: alert exhausted all tiers",
			"id", alert.ID, "type", alert.Type, "device", alert.DeviceIMEI)
		return
	}

	tier := chain.Tiers[alert.CurrentTier]
	alert.State = store.AlertStateEscalating

	// Build template data for Go template-based formatting.
	data := apprise.AlertData{
		AlertID:     alert.ID,
		DeviceIMEI:  alert.DeviceIMEI,
		Type:        alert.Type,
		Detail:      alert.Detail,
		TierName:    tier.Name,
		TierNum:     alert.CurrentTier + 1,
		TierTotal:   len(chain.Tiers),
		Retry:       alert.Retries + 1,
		MaxRetries:  tier.MaxRetries,
		TriggeredAt: alert.CreatedAt.Format(time.RFC3339),
	}
	subject := apprise.FormatSubject(data)
	body := apprise.FormatBody(data)

	// Merge targets: escalation chain tier targets + per-device notification URLs.
	targets := make([]string, len(tier.Targets))
	copy(targets, tier.Targets)
	if alert.DeviceIMEI != "" {
		// Try device-specific prefs first, then tenant-wide default ("*").
		if pref, err := e.store.GetNotificationPref(ctx, "", alert.DeviceIMEI); err == nil && pref.Enabled {
			targets = append(targets, pref.URLs...)
		}
		if pref, err := e.store.GetNotificationPref(ctx, "", "*"); err == nil && pref.Enabled {
			targets = append(targets, pref.URLs...)
		}
	}

	e.mu.Lock()
	notifier := e.notifier
	e.mu.Unlock()

	if err := notifier.Notify(ctx, targets, subject, body); err != nil {
		slog.Error("escalation: notify failed",
			"alert", alert.ID, "tier", tier.Name, "error", err)
	}

	alert.Retries++

	if alert.Retries >= tier.MaxRetries {
		// Move to next tier.
		alert.CurrentTier++
		alert.Retries = 0

		if alert.CurrentTier >= len(chain.Tiers) {
			alert.State = store.AlertStateExhausted
			alert.NextEscAt = now
		} else {
			nextTier := chain.Tiers[alert.CurrentTier]
			alert.NextEscAt = now.Add(time.Duration(nextTier.WaitSec) * time.Second)
		}
	} else {
		// Retry within current tier with exponential backoff.
		backoff := time.Duration(1<<uint(alert.Retries)) * time.Second
		if maxWait := time.Duration(tier.WaitSec) * time.Second; backoff > maxWait {
			backoff = maxWait
		}
		alert.NextEscAt = now.Add(backoff)
	}

	alert.UpdatedAt = now
	if err := e.store.UpdateAlert(ctx, "", alert); err != nil {
		slog.Error("escalation: update alert", "id", alert.ID, "error", err)
	}

	slog.Info("escalation: processed",
		"id", alert.ID, "state", alert.State, "tier", alert.CurrentTier,
		"retries", alert.Retries, "next_esc", alert.NextEscAt)
}
