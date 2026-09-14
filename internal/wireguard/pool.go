package wireguard

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// ClientPool resolves the wg-easy server a tenant's field devices dial into.
//
// Per tenant since MESHSAT-1121. There was one server, one address pool and one
// admin password for the whole deployment, so every tenant's peers sat in one
// place -- and GetPeerConfig hands back a peer's PRIVATE KEY, which is why
// MESHSAT-1116 had to gate the endpoints to platform admins as a stopgap.
//
// Unlike the stateless HTTP pools, a wg-easy Client holds a LOGIN SESSION, so
// building one costs a round trip. The cache keeps it, and a changed password
// rebuilds it because the password is in the fingerprint.
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

// ForTenant returns the tenant's wg-easy client, or nil when it has none.
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
	acct, err := p.accounts.ForTenant(ctx, tenantID, integrations.ProviderWireGuard)
	if err != nil {
		slog.Warn("wireguard: resolving the tenant's account failed", "tenant", tenantID, "error", err)
		return nil
	}
	if acct == nil {
		// The default tenant keeps the environment-configured server; every other
		// tenant gets nil. An explicit tenant comparison, not a nil check on
		// p.platform: the latter would put every customer's devices on the
		// operator's VPN and hand them each other's peer configurations.
		if tenantID == store.DefaultTenantID {
			return p.platform
		}
		return nil
	}
	if acct.Platform && p.platform != nil {
		return p.platform
	}
	url, pass := acct.Get("url"), acct.Get("password")
	if url == "" {
		return nil
	}
	return p.cache.Get(tenantID, integrations.Fingerprint(url, pass), func() *Client {
		// guardOutbound because this URL came from a TENANT. The platform client
		// above is deliberately NOT guarded: its http://wg-easy:51821 is exactly
		// what the guard refuses, and the operator is entitled to it.
		c := NewClient(url, pass).maybeGuard(p.noGuard, 10*time.Second)
		// Log in now rather than on the first device registration, so a wrong
		// password surfaces here with the tenant named instead of as a failed
		// device create somebody has to interpret.
		if err := c.Login(ctx); err != nil {
			slog.Warn("wireguard: this tenant's wg-easy refused the login",
				"tenant", tenantID, "url", url, "error", err)
			return nil
		}
		return c
	})
}

// Provisioner auto-creates WireGuard peers when devices are registered and
// removes them when devices are deleted, on the tenant's OWN server.
type Provisioner struct {
	pool *ClientPool
	mu   sync.RWMutex
	// peers is keyed by tenant AND device. A device id is only unique within a
	// tenant, so a map keyed on the device alone let one tenant's registration
	// overwrite another's peer mapping -- and then a delete removed the wrong
	// tenant's peer.
	peers map[string]string
}

// NewProvisionerPool creates a per-tenant auto-provisioner.
func NewProvisionerPool(pool *ClientPool) *Provisioner {
	return &Provisioner{pool: pool, peers: make(map[string]string)}
}

func peerKey(tenantID, deviceID string) string { return tenantID + "\x00" + deviceID }
