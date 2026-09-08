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
