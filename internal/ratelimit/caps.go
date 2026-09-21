package ratelimit

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// Caps is one tenant's per-device send budget.
type Caps struct {
	Daily   int
	Monthly int
}

// CapResolver returns a tenant's budget. A function type rather than an
// interface so a test can be one line, and so the limiter needs no opinion
// about where a budget comes from.
type CapResolver func(tenantID string) Caps

// tenantLookup is the slice of the store the resolver needs.
type tenantLookup interface {
	GetTenant(ctx context.Context, id string) (*store.Tenant, error)
}

// PlanCaps resolves a tenant's send budget from its plan, with a platform
// admin's per-tenant override on top, and caches the answer.
//
// # Why this exists (MESHSAT-1117 tranche 2c)
//
// HUB_RATELIMIT_DAILY_CAP was one number for the whole platform, so a Fleet
// tenant paying EUR 29 a month got the same 100 messages a day as a free one.
// The cap is the only thing in the product that could give a paid tier meaning
// on the airtime side, and it was inert.
//
// # Why the floor is not optional
//
// A resolved budget is NEVER below the platform default. `plans.SendCaps`
// enforces that, and this type does not undo it. The reason is the invariant
// that outranks the feature: a lapse drops a tenant's plan back to free, and if
// a lower plan meant a smaller budget, a lapse would reduce MESSAGE DELIVERY --
// which is exactly what "the ceiling gates registration and nothing else"
// forbids. Raising the paid tiers gives the tier commercial meaning without
// ever taking delivery away from anybody.
//
// # Why it caches
//
// Allow runs once per outbound message. A database round trip there would put
// the billing system on the send path, which is the shape of problem the SOS
// invariant exists to prevent. Same 30-second TTL as tenancy.Resolver, and a
// lookup failure answers with the platform floor rather than refusing.
type PlanCaps struct {
	store        tenantLookup
	floorDaily   int
	floorMonthly int
	ttl          time.Duration

	mu    sync.RWMutex
	cache map[string]capEntry
	now   func() time.Time
}

type capEntry struct {
	caps Caps
	at   time.Time
}

// NewPlanCaps returns a resolver. floorDaily and floorMonthly are the platform
// values every tenant gets at minimum.
func NewPlanCaps(s tenantLookup, floorDaily, floorMonthly int, ttl time.Duration) *PlanCaps {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &PlanCaps{
		store: s, floorDaily: floorDaily, floorMonthly: floorMonthly, ttl: ttl,
		cache: map[string]capEntry{}, now: time.Now,
	}
}

// Resolve is the CapResolver.
func (p *PlanCaps) Resolve(tenantID string) Caps {
	if p == nil || tenantID == "" {
		return Caps{Daily: 0, Monthly: 0} // the limiter falls back to its own values
	}

	p.mu.RLock()
	e, ok := p.cache[tenantID]
	p.mu.RUnlock()
	if ok && p.now().Sub(e.at) < p.ttl {
		return e.caps
	}

	caps := p.lookup(tenantID)

	p.mu.Lock()
	p.cache[tenantID] = capEntry{caps: caps, at: p.now()}
	p.mu.Unlock()
	return caps
}

func (p *PlanCaps) lookup(tenantID string) Caps {
	floor := Caps{Daily: p.floorDaily, Monthly: p.floorMonthly}
	if p.store == nil {
		return floor
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	t, err := p.store.GetTenant(ctx, tenantID)
	if err != nil || t == nil {
		// The floor, not a refusal. A database blip must not shrink a
		// customer's send budget, and it must certainly not raise it.
		slog.Debug("ratelimit: tenant lookup for send caps failed, using the platform floor",
			"tenant", tenantID, "error", err)
		return floor
	}

	daily, monthly := plans.SendCaps(t.Plan, p.floorDaily, p.floorMonthly)
	// The tenant's own number (set by its owner in Settings, or by a platform
	// admin for them) sits on top of the plan, and is still floored:
	// an override BELOW the platform default would reduce delivery, which is the
	// thing this whole design refuses to do.
	if t.RatelimitDailyCap > daily {
		daily = t.RatelimitDailyCap
	}
	if t.RatelimitMonthlyCap > monthly {
		monthly = t.RatelimitMonthlyCap
	}
	return Caps{Daily: daily, Monthly: monthly}
}

// ForgetTenant drops a tenant's cached budget, so a plan change or an override
// applies now rather than at the end of the TTL. Implements
// tenancy.TenantForgetter.
func (p *PlanCaps) ForgetTenant(tenantID string) {
	if p == nil || tenantID == "" {
		return
	}
	p.mu.Lock()
	delete(p.cache, tenantID)
	p.mu.Unlock()
}
