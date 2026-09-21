package email

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// Notifier implements escalation.Notifier by sending emails to address targets.
// Targets that contain "@" are treated as email addresses; others are skipped.
type Notifier struct {
	client *Client
	pool   *Pool
}

// NewNotifier creates an email escalation notifier bound to one client.
func NewNotifier(client *Client) *Notifier {
	return &Notifier{client: client}
}

// NewNotifierPool creates a notifier that picks the email gateway of the tenant
// carried in the context (tenancy.WithTenant), the shape MESHSAT-977 established
// for SMS.
func NewNotifierPool(pool *Pool) *Notifier {
	return &Notifier{pool: pool}
}

// Notify sends an email to each email address target. Non-email targets are skipped.
//
// The context used to be discarded outright (`_ context.Context`), which is why
// every tenant's alerts left from one address signed with one PGP key: the tenant
// was sitting in the context and nothing read it (MESHSAT-1121).
//
// A call with no email address among the targets returns nil before the
// tenant's gateway is looked at: every backend is handed every target, and a
// chain of phone numbers on a tenant without mail is not a mail failure. It
// was logged as one on every step of a delivered SOS page (MESHSAT-1294).
func (n *Notifier) Notify(ctx context.Context, targets []string, subject, body string) error {
	addrs := make([]string, 0, len(targets))
	for _, t := range targets {
		if isEmailAddress(t) {
			addrs = append(addrs, t)
		}
	}
	if len(addrs) == 0 {
		return nil
	}
	var lastErr error
	sent := 0

	client := n.client
	if n.pool != nil {
		tenantID := tenancy.FromContext(ctx)
		if tenantID == "" {
			tenantID = store.DefaultTenantID
		}
		gw := n.pool.ForTenant(ctx, tenantID)
		if gw == nil {
			return ErrNoGateway
		}
		client = gw.Client
	}
	if client == nil {
		return ErrNoGateway
	}

	for _, target := range addrs {
		if err := client.Send(target, subject, body); err != nil {
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
	// Must contain @, have text before and after, and not be a URL. Any
	// "scheme://" target is an Apprise URL even when it carries credentials
	// ("json://user:pass@host"); listing a few schemes let the others through
	// as email addresses (MESHSAT-1294).
	if !strings.Contains(target, "@") {
		return false
	}
	if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
		return false
	}
	parts := strings.SplitN(target, "@", 2)
	return len(parts) == 2 && parts[0] != "" && strings.Contains(parts[1], ".")
}
