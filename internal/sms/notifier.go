package sms

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// Notifier implements escalation.Notifier by sending SMS to phone number targets.
// Targets that start with "+" are treated as E.164 phone numbers; others are skipped.
type Notifier struct {
	client *Client
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

	for _, target := range targets {
		if !isPhoneNumber(target) {
			continue
		}
		if _, err := n.client.Send(ctx, target, text); err != nil {
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
