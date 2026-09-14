package ntfy

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/meshsat/meshsat-hub/internal/integrations"
	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/tenancy"
)

// errNoNtfyAccount is an error rather than a silent success: a push nobody can
// deliver should be visible, which is exactly what this class of defect was not.
var errNoNtfyAccount = errors.New("ntfy: no push server configured for this tenant (Integrations page)")

// ClientPool resolves the ntfy server a tenant's pushes are published to.
// Same shape as internal/sms/pool.go.
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

// ForTenant returns the tenant's ntfy client, or nil when it has none.
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
	acct, err := p.accounts.ForTenant(ctx, tenantID, integrations.ProviderNtfy)
	if err != nil {
		slog.Warn("ntfy: resolving the tenant's account failed", "tenant", tenantID, "error", err)
		return nil
	}
	if acct == nil {
		// The default tenant still gets the platform server even when SetPlatform
		// was never called; every other tenant gets nil. See the same comment in
		// internal/apprise/pool.go -- the explicit tenant comparison is the
		// isolation boundary and must not become a bare nil check.
		if tenantID == store.DefaultTenantID {
			return p.platform
		}
		return nil
	}
	if acct.Platform && p.platform != nil {
		return p.platform
	}
	url, token := acct.Get("url"), acct.Get("token")
	if url == "" {
		return nil
	}
	// The token is part of the fingerprint, so rotating it rebuilds the client
	// rather than leaving a cached one authenticating with the old value.
	return p.cache.Get(tenantID, integrations.Fingerprint(url, token), func() *Client {
		// Guarded: this URL came from a TENANT (see internal/netguard).
		c := New(url).maybeGuard(p.noGuard, 15*time.Second)
		if token != "" {
			c.SetToken(token)
		}
		return c
	})
}

// Notifier implements escalation.Notifier, picking the ntfy server of the
// tenant carried in the context.
type Notifier struct{ pool *ClientPool }

func NewNotifierPool(pool *ClientPool) *Notifier { return &Notifier{pool: pool} }

func (n *Notifier) Notify(ctx context.Context, targets []string, subject, body string) error {
	tenantID := tenancy.FromContext(ctx)
	if tenantID == "" {
		tenantID = store.DefaultTenantID
	}
	c := n.pool.ForTenant(ctx, tenantID)
	if c == nil {
		return errNoNtfyAccount
	}
	return c.Notify(ctx, targets, subject, body)
}
