// Package quota answers one question: may this tenant register one more device
// or bridge (MESHSAT-989).
//
// It lives outside internal/api because the machine paths need it too -- a
// bridge announcing an unknown device over MQTT is a registration as much as a
// POST is -- and internal/api already imports internal/bridge, so the check
// could not live there.
package quota

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// PlanLookup reports a tenant's subscription plan. Supplied at startup so the
// API does not have to read the tenant row on every create; tenancy.StatusCache
// provides it and already invalidates across replicas when a plan changes.
type PlanLookup func(ctx context.Context, tenantID string) (string, error)

// Checker answers "may this tenant register one more" and nothing else.
//
// It is deliberately not in the middleware. Status is a property of a request;
// a device ceiling is a property of a create, and putting it in the middleware
// would cost two count queries on every call to every endpoint.
//
// It never sees ingest. A tenant at or over its cap keeps receiving from every
// device it already has, keeps its SOS path, and keeps its dead man's switch.
// The only thing it cannot do is add another device.
type Checker struct {
	store store.Store
	plan  PlanLookup
}

// NewQuota returns a quota checker. A nil store or lookup disables it, which is
// what a deployment with no tiers configured wants.
func New(s store.Store, plan PlanLookup) *Checker {
	return &Checker{store: s, plan: plan}
}

// Usage is what a tenant has and what it is allowed.
type Usage struct {
	Plan      string `json:"plan"`
	Devices   int    `json:"devices"`
	Bridges   int    `json:"bridges"`
	Used      int    `json:"used"`      // devices + bridges
	Limit     int    `json:"limit"`     // -1 when the plan has no ceiling
	Remaining int    `json:"remaining"` // -1 when unlimited; never negative otherwise
	OverLimit bool   `json:"over_limit"`
}

// Usage reports a tenant's current standing.
func (q *Checker) Usage(ctx context.Context, tenantID string) (Usage, error) {
	u := Usage{Plan: plans.Free, Limit: plans.Unlimited, Remaining: plans.Unlimited}
	if q == nil || q.store == nil {
		return u, nil
	}
	if q.plan != nil {
		p, err := q.plan(ctx, tenantID)
		if err != nil {
			return u, err
		}
		u.Plan = plans.Normalise(p)
	}
	devices, err := q.store.CountBillableDevices(ctx, tenantID)
	if err != nil {
		return u, err
	}
	bridges, err := q.store.CountBridges(ctx, tenantID)
	if err != nil {
		return u, err
	}
	u.Devices, u.Bridges, u.Used = devices, bridges, devices+bridges
	u.Limit = plans.For(u.Plan).Devices
	if u.Limit == plans.Unlimited {
		u.Remaining = plans.Unlimited
		return u, nil
	}
	if remaining := u.Limit - u.Used; remaining > 0 {
		u.Remaining = remaining
	} else {
		u.Remaining = 0
		u.OverLimit = u.Used > u.Limit
	}
	return u, nil
}

// AllowAnother reports whether the tenant may register one more device or
// bridge, and if not, a message written for the person who will read it.
//
// A lookup failure allows the create. The alternative is refusing to register
// a customer's kit because a count query timed out, and a fleet nobody can add
// to is a worse failure than a tenant briefly one device over.
func (q *Checker) AllowAnother(ctx context.Context, tenantID string) (bool, string) {
	if q == nil || q.store == nil {
		return true, ""
	}
	u, err := q.Usage(ctx, tenantID)
	if err != nil {
		slog.Warn("quota: usage lookup failed, allowing the registration", "tenant", tenantID, "error", err)
		return true, ""
	}
	if plans.AllowsAnother(u.Plan, u.Used) {
		return true, ""
	}
	return false, fmt.Sprintf(
		"the %s plan covers %d devices and bridges together, and you have %d. "+
			"Remove one, or move up a plan to add more.", u.Plan, u.Limit, u.Used)
}
