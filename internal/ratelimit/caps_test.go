package ratelimit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/plans"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1117 tranche 2c. HUB_RATELIMIT_DAILY_CAP was one number for the whole
// platform, so a Fleet tenant paying EUR 29 a month got the same 100 messages a
// day as a free one.
//
// The property that matters most here is the FLOOR: a resolved budget is never
// below the platform default. Without it, a plan lapse would reduce message
// DELIVERY -- and the rule that outranks this feature is that a subscription
// ceiling gates registration and nothing else. Every test below that mentions
// the floor is guarding that, not a rounding detail.

type capStore struct {
	mu      sync.Mutex
	tenants map[string]*store.Tenant
	calls   int
	err     error
}

func (c *capStore) GetTenant(_ context.Context, id string) (*store.Tenant, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	t, ok := c.tenants[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *t
	return &cp, nil
}

func (c *capStore) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// withPlanCaps sets a plan's caps for the duration of a test and restores them,
// because the plans table is package-level state shared by every test.
func withPlanCaps(t *testing.T, plan string, daily, monthly int) {
	t.Helper()
	if !plans.SetSendCaps(plan, daily, monthly) {
		t.Fatalf("unknown plan %q", plan)
	}
	// The whole table, because SetSendCaps ignores a zero and so cannot undo
	// itself. The plans table is package-level state shared by every test.
	t.Cleanup(plans.ResetForTest)
}

func TestAPaidPlanRaisesTheBudget(t *testing.T) {
	withPlanCaps(t, plans.Fleet, 1000, 20000)
	s := &capStore{tenants: map[string]*store.Tenant{
		"t_fleet": {ID: "t_fleet", Plan: plans.Fleet},
		"t_free":  {ID: "t_free", Plan: plans.Free},
	}}
	p := NewPlanCaps(s, 100, 2000, time.Minute)

	if got := p.Resolve("t_fleet"); got.Daily != 1000 || got.Monthly != 20000 {
		t.Errorf("the Fleet tenant resolved to %+v, want 1000/20000. The tier had no effect "+
			"on airtime at all before this.", got)
	}
	if got := p.Resolve("t_free"); got.Daily != 100 || got.Monthly != 2000 {
		t.Errorf("the free tenant resolved to %+v, want the platform 100/2000", got)
	}
}

// The load-bearing test. A plan configured BELOW the platform default must not
// take delivery away -- which is what a lapse would otherwise do.
func TestAPlanCanNeverLowerATenantsBudgetBelowThePlatformDefault(t *testing.T) {
	withPlanCaps(t, plans.Free, 10, 50) // an operator tries to make free tiny
	s := &capStore{tenants: map[string]*store.Tenant{
		"t_free": {ID: "t_free", Plan: plans.Free},
	}}
	p := NewPlanCaps(s, 100, 2000, time.Minute)

	got := p.Resolve("t_free")
	if got.Daily != 100 || got.Monthly != 2000 {
		t.Errorf("resolved to %+v, want the platform floor 100/2000.\n"+
			"A plan that resolves lower means a LAPSE REDUCES MESSAGE DELIVERY, and the "+
			"subscription ceiling is only ever allowed to gate registration.", got)
	}
}

// The same rule, stated as the scenario it protects: a tenant lapses from a
// raised tier back to free and must land on the platform default, not below.
func TestALapseReturnsATenantToThePlatformDefaultAndNoLower(t *testing.T) {
	withPlanCaps(t, plans.Crew, 500, 10000)
	withPlanCaps(t, plans.Free, 5, 20) // misconfigured, deliberately
	tenant := &store.Tenant{ID: "t_lapsing", Plan: plans.Crew}
	s := &capStore{tenants: map[string]*store.Tenant{"t_lapsing": tenant}}
	p := NewPlanCaps(s, 100, 2000, 0) // 0 -> default TTL, but we forget between

	if got := p.Resolve("t_lapsing"); got.Daily != 500 {
		t.Fatalf("while paying: %+v, want 500/day", got)
	}

	// The lapse job drops the plan to free.
	s.mu.Lock()
	tenant.Plan = plans.Free
	s.mu.Unlock()
	p.ForgetTenant("t_lapsing")

	got := p.Resolve("t_lapsing")
	if got.Daily != 100 || got.Monthly != 2000 {
		t.Errorf("after lapsing: %+v, want the platform 100/2000. A lapsed customer must "+
			"not end up able to send LESS than a customer who never paid.", got)
	}
}

// A number set for one tenant is the number in force, higher or lower than the
// platform default (owner ruling, 21 Sep 2026): the tenant pays its own carrier
// for these messages, so a tenant that wants 5 a day to protect its bill gets 5.
// What may still never go below the default is a PLAN, which the tests above
// hold: a lapse is not a choice anybody made.
func TestATenantsOwnNumberIsHonouredHigherOrLower(t *testing.T) {
	s := &capStore{tenants: map[string]*store.Tenant{
		"t_raised":  {ID: "t_raised", Plan: plans.Free, RatelimitDailyCap: 5000},
		"t_lowered": {ID: "t_lowered", Plan: plans.Free, RatelimitDailyCap: 5, RatelimitMonthlyCap: 50},
		"t_unset":   {ID: "t_unset", Plan: plans.Free},
	}}
	p := NewPlanCaps(s, 100, 2000, time.Minute)

	if got := p.Resolve("t_raised"); got.Daily != 5000 || got.Monthly != 2000 {
		t.Errorf("the raised tenant resolved to %+v, want 5000/day and the default 2000/month", got)
	}
	if got := p.Resolve("t_lowered"); got.Daily != 5 || got.Monthly != 50 {
		t.Errorf("a tenant that chose 5/day and 50/month resolved to %+v", got)
	}
	if got := p.Resolve("t_unset"); got.Daily != 100 || got.Monthly != 2000 {
		t.Errorf("a tenant that chose nothing resolved to %+v, want the platform default 100/2000", got)
	}
}

// A database failure must answer with the floor, not with unlimited and not
// with a refusal. Unlimited would hand somebody an unmetered satellite account
// on their own carrier bill.
func TestALookupFailureFallsBackToThePlatformFloor(t *testing.T) {
	s := &capStore{err: errors.New("database is down")}
	p := NewPlanCaps(s, 100, 2000, time.Minute)

	got := p.Resolve("t_any")
	if got.Daily != 100 || got.Monthly != 2000 {
		t.Errorf("resolved to %+v on a failed lookup, want the platform floor 100/2000", got)
	}
}

// Allow runs once per outbound message. A database round trip there would put
// the billing system on the send path.
func TestTheBudgetIsCachedSoAllowMakesNoRoundTrip(t *testing.T) {
	s := &capStore{tenants: map[string]*store.Tenant{"t_a": {ID: "t_a", Plan: plans.Free}}}
	p := NewPlanCaps(s, 100, 2000, time.Minute)

	for i := 0; i < 50; i++ {
		p.Resolve("t_a")
	}
	if n := s.callCount(); n != 1 {
		t.Errorf("the store was consulted %d times for 50 messages, want 1", n)
	}

	p.ForgetTenant("t_a")
	p.Resolve("t_a")
	if n := s.callCount(); n != 2 {
		t.Errorf("after ForgetTenant the store was consulted %d times, want 2 -- a plan "+
			"change would not apply until the TTL expired", n)
	}
}

// The limiter must actually USE the resolver, and a resolver answering 0 means
// "nothing to say", never "unlimited".
func TestTheLimiterUsesTheResolvedBudget(t *testing.T) {
	l := NewDeviceLimiter(1000, 1000, 2, 0, nil) // platform: 2 a day
	l.SetCapResolver(func(tenantID string) Caps {
		if tenantID == "t_generous" {
			return Caps{Daily: 5}
		}
		return Caps{} // nothing to say
	})

	for i := 0; i < 5; i++ {
		if !l.Allow("t_generous", "dev", false) {
			t.Fatalf("the generous tenant was refused at send %d, its budget is 5", i+1)
		}
	}
	if l.Allow("t_generous", "dev", false) {
		t.Error("the generous tenant was allowed a 6th send on a budget of 5")
	}

	for i := 0; i < 2; i++ {
		if !l.Allow("t_plain", "dev", false) {
			t.Fatalf("the plain tenant was refused at send %d, the platform budget is 2", i+1)
		}
	}
	if l.Allow("t_plain", "dev", false) {
		t.Error("a resolver answering 0 was treated as unlimited rather than as " +
			"'use the platform value'")
	}
}

// And the bypass still comes first. A tenant on any budget, exhausted, sends
// its SOS.
func TestTheResolvedBudgetNeverStandsInFrontOfAnSOS(t *testing.T) {
	l := NewDeviceLimiter(1, 0, 1, 0, nil)
	l.SetCapResolver(func(string) Caps { return Caps{Daily: 1, Monthly: 1} })

	exhaust(t, l, "t_a", "dev")
	if !l.Allow("t_a", "dev", true) {
		t.Error("an SOS was refused by a plan-derived budget. Nothing about billing may " +
			"stand between a distress message and the constellation.")
	}
}
