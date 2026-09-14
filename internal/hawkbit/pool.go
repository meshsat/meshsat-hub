package hawkbit

import (
	"context"
	"log/slog"
	"time"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// ClientPool resolves the hawkBit server a tenant's firmware rollouts go
// through, per tenant since MESHSAT-1121.
//
// hawkBit is multi-tenant ITSELF, and that is what makes this cheap. Its
// Management API scopes every request to the tenant of the authenticated
// principal, so "which tenant" is carried entirely by the credentials -- no
// tenant field has to be invented on Target, Rollout or controllerId, and the
// Go client needs no tenant parameter at all.
//
// That matters because OTA is the highest-impact primitive on the router: it
// pushes firmware to hardware in the field. Before this there was one server,
// one account and no ownership model anywhere, so any authenticated member of
// any tenant could start a rollout or cancel another tenant's in-flight update
// -- which is why MESHSAT-1116 gated it to platform admins as a stopgap.
type ClientPool struct {
	platform *Client
	accounts *integrations.Service
	cache    integrations.ClientCache[*Client]

	// noGuard disables the dial-time SSRF guard on clients this pool builds.
	//
	// TEST SEAM, and the only caller is a test in this package. The pool tests point
	// at httptest servers, which bind to 127.0.0.1 -- exactly what the guard refuses
	// and exactly what it should refuse. Those tests are about ROUTING (whose server
	// and whose credentials get used); the guard itself is covered by
	// internal/netguard's own tests, including that it survives DNS rebinding.
	//
	// Deliberately unexported with no production setter, so it cannot be reached
	// from outside this package or switched on by configuration.
	noGuard bool
}

func NewClientPool(platform *Client, accounts *integrations.Service) *ClientPool {
	return &ClientPool{platform: platform, accounts: accounts}
}

// ForTenant returns the tenant's hawkBit client, or nil when it has none.
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
	acct, err := p.accounts.ForTenant(ctx, tenantID, integrations.ProviderHawkbit)
	if err != nil {
		slog.Warn("hawkbit: resolving the tenant's account failed", "tenant", tenantID, "error", err)
		return nil
	}
	if acct == nil {
		// The default tenant keeps the environment-configured server; every other
		// tenant gets nil. An explicit tenant comparison, not a nil check on
		// p.platform: the latter would let one customer push firmware to another
		// customer's fleet.
		if tenantID == store.DefaultTenantID {
			return p.platform
		}
		return nil
	}
	if acct.Platform && p.platform != nil {
		return p.platform
	}
	url, user, pass := acct.Get("url"), acct.Get("username"), acct.Get("password")
	if url == "" || user == "" {
		return nil
	}
	return p.cache.Get(tenantID, integrations.Fingerprint(url, user, pass), func() *Client {
		// Guarded: this URL came from a TENANT (see internal/netguard).
		return NewClient(url, user, pass).maybeGuard(p.noGuard, 30*time.Second)
	})
}
