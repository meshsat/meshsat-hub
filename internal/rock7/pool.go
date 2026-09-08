package rock7

import (
	"context"
	"log/slog"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// ClientPool hands out one Rock7 client per tenant account (MESHSAT-977).
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
	acct, err := p.accounts.ForTenant(ctx, tenantID, integrations.ProviderRock7)
	if err != nil {
		slog.Warn("rock7: account lookup failed", "tenant", tenantID, "error", err)
		return nil
	}
	if acct == nil {
		return nil
	}
	if acct.Platform && p.platform != nil {
		return p.platform
	}
	u, pw := acct.Get("username"), acct.Get("password")
	return p.cache.Get(tenantID, integrations.Fingerprint(u, pw), func() *Client { return NewClient(u, pw) })
}
