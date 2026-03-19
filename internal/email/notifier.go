package email

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// Notifier implements escalation.Notifier by sending emails to address targets.
// Targets that contain "@" are treated as email addresses; others are skipped.
type Notifier struct {
	client *Client
}

// NewNotifier creates an email escalation notifier.
func NewNotifier(client *Client) *Notifier {
	return &Notifier{client: client}
}

// Notify sends an email to each email address target. Non-email targets are skipped.
func (n *Notifier) Notify(_ context.Context, targets []string, subject, body string) error {
	var lastErr error
	sent := 0

	for _, target := range targets {
		if !isEmailAddress(target) {
			continue
		}
		if err := n.client.Send(target, subject, body); err != nil {
			slog.Error("email: escalation notify failed", "to", target, "error", err)
			lastErr = err
		} else {
			sent++
		}
	}

	if sent == 0 && lastErr != nil {
		return fmt.Errorf("email: all sends failed: %w", lastErr)
	}
	return nil
}

// isEmailAddress returns true if the target looks like an email address.
func isEmailAddress(target string) bool {
	// Must contain @, have text before and after, and not start with common URL schemes.
	if !strings.Contains(target, "@") {
		return false
	}
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") ||
		strings.HasPrefix(target, "slack://") || strings.HasPrefix(target, "mailto:") {
		return false
	}
	parts := strings.SplitN(target, "@", 2)
	return len(parts) == 2 && parts[0] != "" && strings.Contains(parts[1], ".")
}
