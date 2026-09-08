package sms

import (
	"context"
	"fmt"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
	"log/slog"
	"strings"
)

// Notifier implements escalation.Notifier by sending SMS to phone number targets.
// Targets that start with "+" are treated as E.164 phone numbers; others are skipped.
type Notifier struct {
	client *Client
	pool   *ClientPool
}

// NewNotifierPool creates a notifier that picks the Twilio account of the
// tenant carried in the context (tenancy.WithTenant), default otherwise.
func NewNotifierPool(pool *ClientPool) *Notifier {
	return &Notifier{pool: pool}
}

// NewNotifier creates an SMS escalation notifier.
func NewNotifier(client *Client) *Notifier {
	return &Notifier{client: client}
}

// Notify sends an SMS to each phone number target. Non-phone targets are skipped.
// The subject and body are concatenated into a concise SMS (truncated to 160 chars).
func (n *Notifier) Notify(ctx context.Context, targets []string, subject, body string) error {
	text := formatSMS(subject, body)
	var lastErr error
	sent := 0

	client := n.client
	if n.pool != nil {
		tenantID := tenancy.FromContext(ctx)
		if tenantID == "" {
			tenantID = store.DefaultTenantID
		}
		client = n.pool.ForTenant(ctx, tenantID)
	}
	if client == nil {
		return fmt.Errorf("sms: no Twilio account configured for this tenant")
	}

	for _, target := range targets {
		if !isPhoneNumber(target) {
			continue
		}
		if _, err := client.Send(ctx, target, text); err != nil {
			slog.Error("sms: escalation notify failed", "to", target, "error", err)
			lastErr = err
		} else {
			sent++
		}
	}

	if sent == 0 && lastErr != nil {
		return fmt.Errorf("sms: all sends failed: %w", lastErr)
	}
	return nil
}

// isPhoneNumber returns true if the target looks like an E.164 phone number.
func isPhoneNumber(target string) bool {
	return strings.HasPrefix(target, "+") && len(target) >= 8 && len(target) <= 16
}

// formatSMS creates a concise alert message within 160 character SMS limit.
func formatSMS(subject, body string) string {
	text := subject
	if body != "" {
		text += " | " + body
	}
	if len(text) > 160 {
		text = text[:157] + "..."
	}
	return text
}
