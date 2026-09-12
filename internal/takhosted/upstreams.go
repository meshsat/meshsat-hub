package takhosted

import (
	"context"
	"log/slog"
	"sync"

	"github.com/meshsat/meshsat-hub/internal/takfront"
)

// Upstreams is every TAK server one tenant's CoT should reach.
//
// Two sources, deliberately additive (MESHSAT-1065):
//
//   - the hosted instance the operator runs for the tenant, resolved by the
//     directory refresher, present only while the TAK front is running;
//   - the server the tenant runs themselves, resolved from their own encrypted
//     settings, which needs no front at all.
//
// Both, when a tenant has both. Choosing one would break the other in a way the
// customer could not diagnose: their kit would vanish from the hosted map their
// phones watch, or never appear on the server they had just configured.
//
// This also means the outbound leg is independent of HUB_TAK_FRONT_ENABLED. The
// front needs a server certificate that does not exist yet; a customer pointing
// the Hub at their own endpoint should not have to wait for it.
type Upstreams struct {
	mu sync.RWMutex
	// hosted is DirectoryRefresher.TenantByID once the front is up, nil otherwise.
	hosted   func(tenantID string) *takfront.Tenant
	external *ExternalUpstreams
	log      *slog.Logger
}

// NewUpstreams wires the resolver. external may be nil.
func NewUpstreams(external *ExternalUpstreams, log *slog.Logger) *Upstreams {
	if log == nil {
		log = slog.Default()
	}
	return &Upstreams{external: external, log: log}
}

// SetHosted attaches the hosted-instance lookup, which only exists once the front
// has built its directory refresher. Until then only tenants' own servers resolve,
// which is correct rather than degraded.
func (u *Upstreams) SetHosted(f func(tenantID string) *takfront.Tenant) {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.hosted = f
	u.mu.Unlock()
}

// For returns every upstream for a tenant, hosted first.
//
// Hosted first because it is the one the tenant's own phones are connected to, so
// if anything is ever made order-sensitive it should favour the map somebody is
// looking at.
func (u *Upstreams) For(ctx context.Context, tenantID string) []*takfront.Tenant {
	if u == nil || tenantID == "" {
		return nil
	}

	u.mu.RLock()
	hosted := u.hosted
	u.mu.RUnlock()

	var out []*takfront.Tenant
	seen := map[string]bool{}

	add := func(t *takfront.Tenant) {
		if t == nil || t.Upstream == "" {
			return
		}
		// De-duplicated by address. A tenant who enters their own hosted instance
		// as "their own server" would otherwise get two connections to it and every
		// marker twice on the same map -- a plausible mistake, and an ugly one to
		// debug from the ATAK end.
		if seen[t.Upstream] {
			return
		}
		seen[t.Upstream] = true
		out = append(out, t)
	}

	if hosted != nil {
		add(hosted(tenantID))
	}
	if u.external != nil {
		t, _ := u.external.For(ctx, tenantID)
		// The reason a misconfigured upstream was refused is logged once per cache
		// interval by the resolver itself, not here: this runs per position.
		add(t)
	}
	return out
}
