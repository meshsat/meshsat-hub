// Package escalation implements the SOS/alert escalation chain engine.
// Alerts progress through ordered notification tiers until acknowledged
// or all tiers are exhausted. Each tier has configurable wait time and
// retry count with exponential backoff.
package escalation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
	"log/slog"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/apprise"
	"github.com/meshsat/meshsat-hub/internal/store"
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

// escalationLease is how long a replica holds an alert step while it
// notifies; the final next_esc_at replaces it once notification returns.
const escalationLease = 2 * time.Minute

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

// isNotFound is how a store says an alert does not exist.
func isNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrNotFound)
}

// TriggerOnce raises alert under its own id and starts escalation. False, and no error,
// when an alert with that id exists already: another replica raised it first (C10, an SOS
// run pages once under a deterministic id).
func (e *Engine) TriggerOnce(ctx context.Context, tenantID string, alert *store.Alert) (bool, error) {
	if alert.ID == "" {
		return false, errors.New("escalation: TriggerOnce needs an alert id")
	}
	if _, err := e.store.GetAlert(ctx, tenantID, alert.ID); err == nil {
		return false, nil
	}
	if err := e.Trigger(ctx, tenantID, alert); err != nil {
		if _, gerr := e.store.GetAlert(ctx, tenantID, alert.ID); gerr == nil {
			return false, nil // lost the race: the other replica's alert stands
		}
		return false, err
	}
	return true, nil
}

// Cancel closes an alert for good: its SOS was cancelled (C10). Escalation stops, nothing
// reopens it (AdvanceAlert only moves an open alert), and AckedBy/AckedAt record who and
// when. False when the alert does not exist or was cancelled already.
func (e *Engine) Cancel(ctx context.Context, tenantID, alertID, by string) (bool, error) {
	alert, err := e.store.GetAlert(ctx, tenantID, alertID)
	if isNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("escalation: get alert: %w", err)
	}
	return e.cancelAlert(ctx, tenantID, alert, by)
}

func (e *Engine) cancelAlert(ctx context.Context, tenantID string, alert *store.Alert, by string) (bool, error) {
	if alert.State == store.AlertStateCancelled {
		return false, nil
	}
	alert.State = store.AlertStateCancelled
	alert.AckedBy = by
	alert.AckedAt = time.Now().UTC()
	alert.UpdatedAt = alert.AckedAt
	if err := e.store.UpdateAlert(ctx, tenantID, alert); err != nil {
		return false, fmt.Errorf("escalation: update alert: %w", err)
	}
	slog.Info("escalation: alert cancelled", "id", alert.ID, "by", by)
	return true, nil
}

// closeRounds bounds Close's retries when a raise keeps winning the insert.
const closeRounds = 3

// ClosedBeforeRaised ends the text of an alert Close wrote closed: it never paged anybody.
const ClosedBeforeRaised = " (cancelled before it paged)"

// Close closes the alert under alert.ID for good, also when it does not exist yet: it is then
// written cancelled (a tombstone), so a start or a fallback that raises it later finds it and
// pages nobody (TriggerOnce never replaces an alert). The id's uniqueness decides every race: a
// raise that inserted first is cancelled here, one that comes later meets the tombstone
// (C10-R6). True when this call closed the alert or wrote its tombstone.
func (e *Engine) Close(ctx context.Context, tenantID string, alert *store.Alert, by string) (bool, error) {
	if alert.ID == "" {
		return false, errors.New("escalation: Close needs an alert id")
	}
	for range closeRounds {
		existing, err := e.store.GetAlert(ctx, tenantID, alert.ID)
		if err == nil {
			return e.cancelAlert(ctx, tenantID, existing, by)
		}
		if !isNotFound(err) {
			return false, fmt.Errorf("escalation: get alert: %w", err)
		}
		now := time.Now().UTC()
		tomb := *alert
		tomb.State = store.AlertStateCancelled
		tomb.Detail += ClosedBeforeRaised
		tomb.CurrentTier, tomb.Retries = 0, 0
		tomb.AckedBy, tomb.AckedAt = by, now
		tomb.CreatedAt, tomb.UpdatedAt, tomb.NextEscAt = now, now, now
		if err := e.store.CreateAlert(ctx, tenantID, &tomb); err == nil {
			slog.Info("escalation: alert closed before it was raised", "id", alert.ID, "by", by)
			return true, nil
		}
		// A raise inserted it after the read: the next round cancels that one.
	}
	return false, fmt.Errorf("escalation: closing alert %s lost %d races to a raise", alert.ID, closeRounds)
}

// processAlerts checks all active alerts and escalates as needed.
func (e *Engine) processAlerts(ctx context.Context) {
	// List active alerts across all tenants. Each alert carries its TenantID,
	// which is used for tenant-scoped store calls during processing.
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

// Step runs one escalation step of a due alert at now, as the loop does for each due alert;
// the store conformance tests drive it on every driver.
func (e *Engine) Step(ctx context.Context, alert *store.Alert, now time.Time) {
	e.processAlert(ctx, alert, now)
}

func (e *Engine) processAlert(ctx context.Context, alert *store.Alert, now time.Time) {
	// Load the chain to get tier configuration.
	chain, err := e.store.GetEscalationChain(ctx, alert.TenantID, alert.ChainID)
	if err != nil {
		// Exhaust the alert rather than returning. Returning leaves NextEscAt in
		// the past, so the alert stays due and this runs again every interval --
		// for ever, at Error level, for an alert that can never be delivered.
		//
		// This is reachable in ordinary use, not just from corruption: there is
		// no UpdateEscalationChain at any layer, so editing a chain means delete
		// and recreate, which orphans every alert already pointing at the old id
		// (MESHSAT-1115).
		// Compare-and-set, like every write of this worker: an alert acknowledged or
		// cancelled since it was listed stays so (C10, r16 review C10-R1).
		expected := alert.NextEscAt
		alert.State = store.AlertStateExhausted
		alert.UpdatedAt = now
		won, uerr := e.store.AdvanceAlert(ctx, alert.TenantID, alert, expected)
		if uerr != nil {
			// Keep the old behaviour on a write failure: better to retry the
			// loop than to drop the alert silently.
			slog.Error("escalation: chain missing AND the alert could not be closed; will retry",
				"id", alert.ID, "chain_id", alert.ChainID, "error", err, "update_error", uerr)
			return
		}
		if !won {
			return // acknowledged, cancelled or advanced meanwhile
		}
		slog.Error("escalation: chain not found, alert closed undelivered",
			"id", alert.ID, "tenant", alert.TenantID, "type", alert.Type,
			"chain_id", alert.ChainID, "error", err)
		return
	}

	if alert.CurrentTier >= len(chain.Tiers) {
		// All tiers exhausted.
		expected := alert.NextEscAt
		alert.State = store.AlertStateExhausted
		alert.UpdatedAt = now
		if won, uerr := e.store.AdvanceAlert(ctx, alert.TenantID, alert, expected); uerr != nil || !won {
			return // a write failure is retried next tick; !won: closed or advanced meanwhile
		}
		slog.Warn("escalation: alert exhausted all tiers",
			"id", alert.ID, "type", alert.Type, "device", alert.DeviceIMEI)
		return
	}

	tier := chain.Tiers[alert.CurrentTier]

	// Take a short lease on the alert with a compare-and-set on next_esc_at:
	// the replica that moves next_esc_at forward is the only one that
	// notifies this step; the others see a future next_esc_at and skip. If
	// this replica dies mid-step the lease expires and the step is retried.
	expected := alert.NextEscAt
	alert.State = store.AlertStateEscalating
	alert.NextEscAt = now.Add(escalationLease)
	alert.UpdatedAt = now
	won, err := e.store.AdvanceAlert(ctx, alert.TenantID, alert, expected)
	if err != nil {
		slog.Error("escalation: lease failed", "alert", alert.ID, "error", err)
		return
	}
	if !won {
		slog.Debug("escalation: step taken by another replica", "alert", alert.ID)
		return
	}
	// The step is finished against this lease only: a cancel or an acknowledgement while
	// the notifier ran, or another replica that took the step after the lease ran out,
	// makes the completion a no-op instead of reopening the alert (r16 review C10-R1).
	lease := alert.NextEscAt

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
		if pref, err := e.store.GetNotificationPref(ctx, alert.TenantID, alert.DeviceIMEI); err == nil && pref.Enabled {
			targets = append(targets, pref.URLs...)
		}
		if pref, err := e.store.GetNotificationPref(ctx, alert.TenantID, "*"); err == nil && pref.Enabled {
			targets = append(targets, pref.URLs...)
		}
	}

	e.mu.Lock()
	notifier := e.notifier
	e.mu.Unlock()

	// The notifier picks the tenant's own provider account (Twilio) from the
	// context (MESHSAT-977); the alert knows its tenant.
	notifyCtx := tenancy.WithTenant(ctx, alert.TenantID)
	if err := notifier.Notify(notifyCtx, targets, subject, body); err != nil {
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
		// Shift is clamped: at 2^16 s the tier cap below always wins, and an
		// unbounded shift wrapped to zero (see dbwrap, MESHSAT-833).
		shift := alert.Retries
		if shift < 0 {
			shift = 0
		}
		if shift > 16 {
			shift = 16
		}
		backoff := time.Duration(1<<shift) * time.Second
		if maxWait := time.Duration(tier.WaitSec) * time.Second; backoff > maxWait {
			backoff = maxWait
		}
		alert.NextEscAt = now.Add(backoff)
	}

	alert.UpdatedAt = now
	done, err := e.store.AdvanceAlert(ctx, alert.TenantID, alert, lease)
	if err != nil {
		// The lease runs out and the step is retried.
		slog.Error("escalation: update alert", "id", alert.ID, "error", err)
		return
	}
	if !done {
		slog.Info("escalation: alert closed or taken over during its step; left as it is", "id", alert.ID)
		return
	}

	slog.Info("escalation: processed",
		"id", alert.ID, "state", alert.State, "tier", alert.CurrentTier,
		"retries", alert.Retries, "next_esc", alert.NextEscAt)
}
