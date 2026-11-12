// Package plans is the tier table: what each subscription plan is called and
// how many devices it allows (MESHSAT-989).
//
// The meter is device count, and that is a deliberate choice about what this
// product sells. Airtime is not ours: every tenant brings its own Cloudloop,
// Twilio and Rock7 accounts and its carrier bills it directly, so there is no
// usage of ours to meter. What we sell is fleet management, and fleet size is
// what scales both the value a tenant gets and what it costs us to store
// positions and hold broker sessions.
//
// The cap governs REGISTRATION ONLY. It never blocks ingest, never blocks
// delivery, and never blocks an SOS. A tenant that lapses or downgrades keeps
// every device it has and keeps hearing from all of them; it simply cannot add
// another until it makes room or upgrades. Emergency traffic from a field kit
// is not a lever to collect payment with.
package plans

import "strings"

// Plan names. These are stored in tenants.plan.
const (
	Free   = "free"
	Crew   = "crew"
	Fleet  = "fleet"
	Custom = "custom"
	// Beta is what every tenant created before tiers existed carries. It is
	// treated as Custom so nothing already running is capped by surprise.
	Beta = "beta"
)

// Unlimited is the cap for plans that have none.
const Unlimited = -1

// Limits is what a plan allows.
type Limits struct {
	// Devices is the combined ceiling on registered devices and bridges: one
	// number, because that is what a customer can count against their own kit.
	Devices int
	// DailyCap and MonthlyCap are the tenant's per-device send budget
	// (MESHSAT-1117 tranche 2c). 0 means this plan says nothing and the
	// platform value (HUB_RATELIMIT_DAILY_CAP / _MONTHLY_CAP) applies, which
	// is what every plan does until an operator sets one -- so adding this
	// changed nothing for anybody on the day it shipped.
	//
	// Unlike Devices, these are NEVER allowed to resolve BELOW the platform
	// value. A plan can raise a tenant's budget, never lower it. The reason is
	// the invariant that outranks the feature: a lapse drops a tenant's plan,
	// and if a lower plan meant a smaller send budget then a lapse would
	// reduce message delivery -- which is exactly what "the ceiling gates
	// registration and nothing else" forbids. Raising paid tiers gives the
	// tier commercial meaning without ever taking delivery away.
	DailyCap   int
	MonthlyCap int
}

// defaults are overridden from config at startup, so a tier can be re-priced
// without a deploy.
//
// Custom and Beta take a real TAK ceiling rather than Unlimited, unlike their
// device allowance. Every TAK user is a long-lived TLS session and a row in
// somebody else's Python process on the control-plane tier, so the ceiling here
// protects the cluster rather than the invoice; a tenant that genuinely needs
// more gets it raised deliberately.
var defaults = map[string]Limits{
	Free:   {Devices: 4},
	Crew:   {Devices: 24},
	Fleet:  {Devices: 100},
	Custom: {Devices: Unlimited},
	Beta:   {Devices: Unlimited},
}

var table = cloneDefaults()

// ResetForTest restores every plan's built-in limits.
//
// Exported because the table is package-level state that startup mutates, and a
// test in ANOTHER package that raises a tier has no other way to put it back --
// SetSendCaps and SetLimit deliberately ignore a zero, so they cannot undo
// themselves. Tests only, same shape as webhook.AllowLoopbackTargetsForTest.
func ResetForTest() { table = cloneDefaults() }

func cloneDefaults() map[string]Limits {
	out := make(map[string]Limits, len(defaults))
	for k, v := range defaults {
		out[k] = v
	}
	return out
}

// SetSendCaps overrides a plan's per-device send budget. Called once at
// startup from config; an unknown plan name is ignored rather than invented.
//
// Like SetLimit it changes FIELDS rather than replacing the struct -- see the
// comment there for the bug that caused.
func SetSendCaps(plan string, daily, monthly int) bool {
	plan = Normalise(plan)
	l, ok := table[plan]
	if !ok {
		return false
	}
	if daily > 0 {
		l.DailyCap = daily
	}
	if monthly > 0 {
		l.MonthlyCap = monthly
	}
	table[plan] = l
	return true
}

// SendCaps returns the plan's send budget, never lower than the platform
// floor. A plan that says nothing (0) resolves to the floor.
//
// The floor is the whole safety property here: whatever an operator puts in the
// config, a tenant's budget can only ever be raised above what every tenant
// gets today, so a plan change -- including a lapse -- cannot reduce delivery.
func SendCaps(plan string, floorDaily, floorMonthly int) (daily, monthly int) {
	l := For(plan)
	daily, monthly = l.DailyCap, l.MonthlyCap
	if daily < floorDaily {
		daily = floorDaily
	}
	if monthly < floorMonthly {
		monthly = floorMonthly
	}
	return daily, monthly
}

// SetLimit overrides a plan's device ceiling. Called once at startup from
// config; a plan name that is not known is ignored rather than invented, so a
// typo in an env var cannot create a tier nobody can be moved off.
//
// It reads the existing entry and changes ONE field. It once assigned a whole
// fresh `Limits{Devices: devices}`, which silently zeroed every other field of
// the tier (the TAK account ceiling, until MESHSAT-1466 retired it; the send
// caps today). Change a field, never replace the struct.
func SetLimit(plan string, devices int) bool {
	plan = Normalise(plan)
	l, ok := table[plan]
	if !ok {
		return false
	}
	l.Devices = devices
	table[plan] = l
	return true
}

// Normalise maps whatever is in the database to a plan name.
func Normalise(plan string) string {
	p := strings.ToLower(strings.TrimSpace(plan))
	if p == "" {
		return Free
	}
	return p
}

// Known reports whether a plan name is one we recognise. Used to validate what
// a platform admin sets, which until now accepted any string under 32 bytes.
func Known(plan string) bool {
	_, ok := table[Normalise(plan)]
	return ok
}

// Names lists the plans, for error messages and the admin API.
func Names() []string { return []string{Free, Crew, Fleet, Custom} }

// For returns the limits of a plan. An unknown plan gets the free limits: a
// tenant carrying a name we do not recognise is a mistake to notice, not a
// reason to hand out an unlimited fleet.
func For(plan string) Limits {
	if l, ok := table[Normalise(plan)]; ok {
		return l
	}
	return table[Free]
}

// AllowsAnother reports whether a tenant on this plan may register one more
// device, given how many it already has.
func AllowsAnother(plan string, current int) bool {
	l := For(plan)
	return l.Devices == Unlimited || current < l.Devices
}
