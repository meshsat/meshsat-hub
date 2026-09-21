package apprise

import (
	"context"
	"log/slog"
	"strings"
	"time"

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
	return p.cache.Get(tenantID, integrations.Fingerprint(url), func() *Client { return New(url).maybeGuard(p.noGuard, 30*time.Second) })
}

// Notifier implements escalation.Notifier, picking the Apprise server of the
// tenant carried in the context.
type Notifier struct{ pool *ClientPool }

func NewNotifierPool(pool *ClientPool) *Notifier { return &Notifier{pool: pool} }

// Notify delivers the targets that are Apprise URLs to the tenant's own Apprise
// server.
//
// Only a "scheme://..." target is Apprise's. The escalation engine hands every
// backend the whole list, and phone numbers and email addresses have backends
// of their own; passing them on made Apprise the second recipient of every SMS
// page's text (MESHSAT-1294). A list without an Apprise URL returns nil.
//
// An unconfigured tenant WITH an Apprise URL returns an error rather than nil. The escalation engine
// logs it, which is the point: this whole class of defect was silence. A tenant
// saved notification URLs, everything answered 200, and nothing was delivered
// because no backend existed anywhere (MESHSAT-1121).
func (n *Notifier) Notify(ctx context.Context, targets []string, subject, body string) error {
	urls := make([]string, 0, len(targets))
	for _, t := range targets {
		if IsURL(t) {
			urls = append(urls, t)
		}
	}
	if len(urls) == 0 {
		return nil
	}
	tenantID := tenancy.FromContext(ctx)
	if tenantID == "" {
		tenantID = store.DefaultTenantID
	}
	c := n.pool.ForTenant(ctx, tenantID)
	if c == nil {
		return errNoAppriseAccount
	}
	return c.Notify(ctx, urls, subject, body)
}

// IsURL reports whether an escalation target is an Apprise URL: a scheme, then
// "://". Every Apprise service is addressed that way (tgram://, mailto://,
// slack://, json://, ...).
func IsURL(target string) bool {
	i := strings.Index(target, "://")
	if i <= 0 {
		return false
	}
	for _, r := range target[:i] {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.'
		if !ok {
			return false
		}
	}
	return true
}
