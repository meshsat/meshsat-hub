package deadman

import (
	"context"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1118. Monitor carried a single tenantID field, hardcoded to
// store.DefaultTenantID in NewMonitor, and every configuration method used it.
// So a customer configuring, listing, snoozing or deleting their own dead man's
// switch was reading and writing the DEFAULT tenant's rows.
//
// That is a life-safety path, and it got sharper the same day: MESHSAT-1115
// made SOS escalation actually page a human, so a snooze or a delete from one
// tenant is a page that does not happen for another.
//
// The background scan was always correct -- it reads every tenant's rows and
// triggers with cfg.TenantID -- so these tests cover the configuration API,
// which is where the bug lived.

const (
	tenantA = "t_alpha"
	tenantB = "t_beta"
)

func cfgFor(imei string) Config {
	return Config{
		DeviceIMEI: imei,
		ChainID:    "chain-1",
		Interval:   time.Hour,
		Grace:      10 * time.Minute,
		Enabled:    true,
	}
}

func TestOneTenantsSwitchIsInvisibleToAnother(t *testing.T) {
	m := NewMonitor(newTestStore(t), nil)

	m.Configure(tenantA, cfgFor("dev-a"))
	m.Configure(tenantB, cfgFor("dev-b"))

	a := m.ListConfigs(tenantA)
	if len(a) != 1 || a[0].DeviceIMEI != "dev-a" {
		t.Fatalf("tenant A sees %v, want only dev-a -- listing used to return every tenant's devices", a)
	}
	b := m.ListConfigs(tenantB)
	if len(b) != 1 || b[0].DeviceIMEI != "dev-b" {
		t.Fatalf("tenant B sees %v, want only dev-b", b)
	}
}

// The sharp one: a delete must not reach across tenants.
func TestATenantCannotDeleteAnothersSwitch(t *testing.T) {
	m := NewMonitor(newTestStore(t), nil)
	// The VICTIM is the default tenant deliberately. That is the shape of the
	// original bug -- every customer action landed on store.DefaultTenantID --
	// and a test using two non-default tenants cannot see a regression to it:
	// the misdirected delete would hit a row that does not exist and the victim
	// would survive by accident. Mutation testing caught exactly that.
	m.Configure(store.DefaultTenantID, cfgFor("shared-imei"))

	m.Remove(tenantB, "shared-imei")

	if got := m.ListConfigs(store.DefaultTenantID); len(got) != 1 {
		t.Error("the default tenant's dead man's switch was removed by another tenant. " +
			"On this path that is a distress alert that will not fire.")
	}
}

// And a snooze must not silence somebody else's.
func TestATenantCannotSnoozeAnothersSwitch(t *testing.T) {
	s := newTestStore(t)
	m := NewMonitor(s, nil)
	// Default tenant as the victim, for the reason given on the delete test.
	m.Configure(store.DefaultTenantID, cfgFor("shared-imei"))

	m.Snooze(tenantB, "shared-imei", time.Hour)

	got, err := s.GetDeadmanConfig(context.Background(), store.DefaultTenantID, "shared-imei")
	if err != nil {
		t.Fatalf("the victim's config: %v", err)
	}
	if !got.SnoozedUntil.IsZero() {
		t.Errorf("the default tenant's switch was snoozed until %v by another tenant", got.SnoozedUntil)
	}
}

// A check-in from an ingest path must credit the right tenant, or a device that
// is reporting normally still looks silent to its owner.
func TestCheckInCreditsTheDevicesOwnTenant(t *testing.T) {
	s := newTestStore(t)
	m := NewMonitor(s, nil)
	m.Configure(tenantA, cfgFor("dev-a"))

	// Mark it alerted, then check in as tenant A: the alert must clear.
	c, err := s.GetDeadmanConfig(context.Background(), tenantA, "dev-a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	c.Alerted = true
	if err := s.SaveDeadmanConfig(context.Background(), tenantA, c); err != nil {
		t.Fatalf("save: %v", err)
	}

	m.CheckIn(tenantB, "dev-a") // wrong tenant: must not clear A's alert
	after, _ := s.GetDeadmanConfig(context.Background(), tenantA, "dev-a")
	if !after.Alerted {
		t.Error("a check-in credited to the wrong tenant cleared another tenant's alert")
	}

	m.CheckIn(tenantA, "dev-a") // right tenant: must clear
	after, _ = s.GetDeadmanConfig(context.Background(), tenantA, "dev-a")
	if after.Alerted {
		t.Error("a check-in from the device's own tenant did not clear its alert")
	}
}

// The constructor must no longer carry a tenant at all. If somebody
// reintroduces a default, every method silently goes back to writing one
// tenant's rows and the tests above would still pass for tenant A.
func TestTheMonitorHoldsNoTenantOfItsOwn(t *testing.T) {
	m := NewMonitor(newTestStore(t), nil)
	// Configure under a tenant that is NOT the default, then confirm the default
	// tenant sees nothing. A monitor with a hardcoded default would show it.
	m.Configure(tenantA, cfgFor("dev-a"))
	if got := m.ListConfigs(store.DefaultTenantID); len(got) != 0 {
		t.Errorf("the default tenant sees %v; the monitor is still writing somebody's rows "+
			"into store.DefaultTenantID", got)
	}
}
