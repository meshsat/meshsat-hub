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
}

// defaults are overridden from config at startup, so a tier can be re-priced
// without a deploy.
var defaults = map[string]Limits{
	Free:   {Devices: 4},
	Crew:   {Devices: 24},
	Fleet:  {Devices: 100},
	Custom: {Devices: Unlimited},
	Beta:   {Devices: Unlimited},
}

var table = cloneDefaults()

func cloneDefaults() map[string]Limits {
	out := make(map[string]Limits, len(defaults))
	for k, v := range defaults {
		out[k] = v
	}
	return out
}

// SetLimit overrides a plan's device ceiling. Called once at startup from
// config; a plan name that is not known is ignored rather than invented, so a
// typo in an env var cannot create a tier nobody can be moved off.
func SetLimit(plan string, devices int) bool {
	plan = Normalise(plan)
	if _, ok := table[plan]; !ok {
		return false
	}
	table[plan] = Limits{Devices: devices}
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
