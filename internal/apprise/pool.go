package apprise

import (
	"context"
	"log/slog"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// ClientPool resolves the Apprise server a tenant's alerts are delivered
// through. Same shape as internal/sms/pool.go, which MESHSAT-977 established.
type ClientPool struct {
	platform *Client
	accounts *integrations.Service
	cache    integrations.ClientCache[*Client]
}

func NewClientPool(platform *Client, accounts *integrations.Service) *ClientPool {
	return &ClientPool{platform: platform, accounts: accounts}
}

// ForTenant returns the tenant's Apprise client, or nil when it has none.
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
	acct, err := p.accounts.ForTenant(ctx, tenantID, integrations.ProviderApprise)
	if err != nil {
		slog.Warn("apprise: resolving the tenant's account failed", "tenant", tenantID, "error", err)
		return nil
	}
	if acct == nil {
		// No stored row. The DEFAULT tenant still gets the platform relay, because
		// the environment values are that tenant's own account -- and this does not
		// depend on SetPlatform having been called, which is a registration step a
		// caller can forget. A caller that forgot it would otherwise build a pool
		// holding a perfectly good relay and deliver nothing through it.
		//
		// Every OTHER tenant gets nil. That is the isolation boundary, and it is
		// the reason this is written as an explicit tenant comparison rather than
		// an `if p.platform != nil` that would quietly serve everybody.
		if tenantID == store.DefaultTenantID {
			return p.platform
		}
		return nil
	}
	if acct.Platform && p.platform != nil {
		return p.platform
	}
	url := acct.Get("url")
	if url == "" {
		return nil
	}
	return p.cache.Get(tenantID, integrations.Fingerprint(url), func() *Client { return New(url) })
}

// Notifier implements escalation.Notifier, picking the Apprise server of the
// tenant carried in the context.
type Notifier struct{ pool *ClientPool }

func NewNotifierPool(pool *ClientPool) *Notifier { return &Notifier{pool: pool} }

// Notify delivers to the tenant's own Apprise server.
//
// An unconfigured tenant returns an error rather than nil. The escalation engine
// logs it, which is the point: this whole class of defect was silence. A tenant
// saved notification URLs, everything answered 200, and nothing was delivered
// because no backend existed anywhere (MESHSAT-1121).
func (n *Notifier) Notify(ctx context.Context, targets []string, subject, body string) error {
	tenantID := tenancy.FromContext(ctx)
	if tenantID == "" {
		tenantID = store.DefaultTenantID
	}
	c := n.pool.ForTenant(ctx, tenantID)
	if c == nil {
		return errNoAppriseAccount
	}
	return c.Notify(ctx, targets, subject, body)
}
