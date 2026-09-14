package email

import (
	"context"
	"errors"
	"log/slog"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// ErrNoGateway means this tenant has no outgoing mail configured. It is an error
// rather than a quiet no-op: an alert nobody can deliver should be visible.
var ErrNoGateway = errors.New("email: no outgoing mail configured for this tenant (Integrations page)")

// Gateway is one tenant's complete email setup: where its mail is sent from, and
// the PGP keys it signs with and encrypts to.
//
// The KeyRing is PER TENANT and that is the point. It used to be one global
// keyring whose contacts map was keyed by bare email address, so two tenants
// adding a contact for the same address collided -- the second overwrote the
// first, and one tenant's alert could then be encrypted to a key the OTHER
// tenant supplied and could read. Splitting the keyring fixes that by
// construction rather than by re-keying a shared map (MESHSAT-1121).
//
// Contacts are still in-memory and still per-replica; that is MESHSAT-1123 and is
// unchanged here.
type Gateway struct {
	Client  *Client
	KeyRing *KeyRing
}

// Pool resolves a tenant's email gateway.
type Pool struct {
	platform *Gateway
	accounts *integrations.Service
	cache    integrations.ClientCache[*Gateway]
}

func NewPool(platform *Gateway, accounts *integrations.Service) *Pool {
	return &Pool{platform: platform, accounts: accounts}
}

// Platform returns the operator's own gateway, or nil. Used by the parts of the
// Hub that are genuinely platform-level rather than per tenant.
func (p *Pool) Platform() *Gateway {
	if p == nil {
		return nil
	}
	return p.platform
}

// ForTenant returns the tenant's gateway, or nil when it has no outgoing mail.
func (p *Pool) ForTenant(ctx context.Context, tenantID string) *Gateway {
	if p == nil {
		return nil
	}
	if tenantID == "" {
		tenantID = store.DefaultTenantID
	}
	if p.accounts == nil {
		return p.platform
	}
	acct, err := p.accounts.ForTenant(ctx, tenantID, integrations.ProviderEmail)
	if err != nil {
		slog.Warn("email: resolving the tenant's account failed", "tenant", tenantID, "error", err)
		return nil
	}
	if acct == nil {
		// The default tenant keeps the environment-configured gateway; every
		// other tenant gets nil. An explicit tenant comparison, not a nil check
		// on p.platform, because the latter would hand the operator's sending
		// identity and PGP key to every customer.
		if tenantID == store.DefaultTenantID {
			return p.platform
		}
		return nil
	}
	if acct.Platform && p.platform != nil {
		return p.platform
	}

	host := acct.Get("smtp_host")
	if host == "" {
		// An inbound-only account: a webhook secret and nothing to send with.
		return nil
	}
	from, user, pass, key := acct.Get("from"), acct.Get("username"), acct.Get("password"), acct.Get("pgp_key")

	return p.cache.Get(tenantID, integrations.Fingerprint(host, from, user, pass, key), func() *Gateway {
		kr, err := NewKeyRing("MeshSat Hub", from, key)
		if err != nil {
			// A malformed key must not silently downgrade to no PGP at all: it
			// would send in cleartext for as long as nobody read the logs.
			slog.Error("email: the tenant's PGP key could not be loaded, no gateway",
				"tenant", tenantID, "error", err)
			return nil
		}
		return &Gateway{Client: NewClient(host, from, user, pass, kr), KeyRing: kr}
	})
}
