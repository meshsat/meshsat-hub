package sms

import (
	"context"
	"log/slog"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// ClientPool hands out one Twilio client per tenant account (MESHSAT-977);
// the platform client serves the default tenant without an account.
type ClientPool struct {
	platform *Client
	accounts *integrations.Service
	cache    integrations.ClientCache[*Client]
	// WhatsApp rides the same Twilio account with its own sender number
	// (MESHSAT-1367), so it is its own client per tenant and its own cache.
	platformWA *Client
	waCache    integrations.ClientCache[*Client]
}

// SetPlatformWhatsApp gives the pool the platform's own WhatsApp client, the
// one the default tenant sends on.
func (p *ClientPool) SetPlatformWhatsApp(c *Client) { p.platformWA = c }

// WhatsAppForTenant returns the tenant's WhatsApp client or nil when it has
// no Twilio account. The sender is the account's whatsapp_from, or its SMS
// From number when none is set.
func (p *ClientPool) WhatsAppForTenant(ctx context.Context, tenantID string) *Client {
	if p == nil {
		return nil
	}
	if tenantID == "" {
		tenantID = store.DefaultTenantID
	}
	if p.accounts == nil {
		return p.platformWA
	}
	acct, err := p.accounts.ForTenant(ctx, tenantID, integrations.ProviderTwilio)
	if err != nil {
		slog.Warn("whatsapp: account lookup failed", "tenant", tenantID, "error", err)
		return nil
	}
	if acct == nil {
		return nil
	}
	if acct.Platform && p.platformWA != nil {
		return p.platformWA
	}
	sid, tok, from := acct.Get("account_sid"), acct.Get("auth_token"), acct.Get("whatsapp_from")
	if from == "" {
		from = acct.Get("from_number")
	}
	return p.waCache.Get(tenantID, integrations.Fingerprint(sid, tok, from), func() *Client {
		c := NewClient(sid, tok, from)
		c.SetChannel("whatsapp")
		return c
	})
}

// NewClientPool creates a pool; accounts nil = platform client for everyone.
func NewClientPool(platform *Client, accounts *integrations.Service) *ClientPool {
	return &ClientPool{platform: platform, accounts: accounts}
}

// ForTenant returns the tenant's client or nil when it has none.
func (p *ClientPool) ForTenant(ctx context.Context, tenantID string) *Client {
	if p == nil {
		return nil
	}
	if tenantID == "" {
		tenantID = store.DefaultTenantID
	}
	if p.accounts == nil {
		return p.platform
	}
	acct, err := p.accounts.ForTenant(ctx, tenantID, integrations.ProviderTwilio)
	if err != nil {
		slog.Warn("sms: account lookup failed", "tenant", tenantID, "error", err)
		return nil
	}
	if acct == nil {
		return nil
	}
	if acct.Platform && p.platform != nil {
		return p.platform
	}
	sid, tok, from := acct.Get("account_sid"), acct.Get("auth_token"), acct.Get("from_number")
	return p.cache.Get(tenantID, integrations.Fingerprint(sid, tok, from), func() *Client { return NewClient(sid, tok, from) })
}
