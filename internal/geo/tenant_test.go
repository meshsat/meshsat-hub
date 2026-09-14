package geo

import (
	"context"
	"testing"
	"time"
)

// MESHSAT-1118. geo.Fence has carried a TenantID field since it was written and
// nothing ever set it or read it, so every map in the engine was one shared
// namespace:
//
//   - ListFences returned every tenant's fences. A fence polygon is the area a
//     customer operates in, and its chain_id names their escalation chain.
//   - RemoveFence deleted any fence by id, whoever owned it.
//   - Evaluate checked a position against EVERY tenant's fences, so one tenant's
//     vehicle crossing another tenant's boundary raises that tenant's chain --
//     and since MESHSAT-1115 a chain sends a real SMS to a real person.
//   - Crossing state was keyed on the device id alone, and device ids are not
//     unique across tenants.
//
// The victim is "default" throughout, deliberately: the bug had no tenant
// dimension at all, so a regression collapses to one namespace, and two
// non-default tenants would let a regression to the default tenant pass by
// accident (the trap mutation testing caught in internal/deadman).
//
// Separately: nothing in the Hub calls Evaluate at all. That is MESHSAT-1119.

const otherTenant = "t_beta"

// box is a 1x1 degree square at the origin. (geofence_test.go already has a
// package-level `square` variable.)
func box() []Point {
	return []Point{{Lat: 0, Lon: 0}, {Lat: 0, Lon: 1}, {Lat: 1, Lon: 1}, {Lat: 1, Lon: 0}}
}

func fenceIn(tenantID, id string) Fence {
	return Fence{
		ID: id, TenantID: tenantID, Name: id, Polygon: box(),
		Trigger: TriggerBoth, ChainID: "chain-" + tenantID, Enabled: true,
	}
}

func TestADeviceCannotCrossAnotherTenantsFence(t *testing.T) {
	e := NewEngine()
	e.AddFence(fenceIn(testTenant, "victim-fence"))
	e.AddFence(fenceIn(otherTenant, "other-fence"))

	// A device belonging to the OTHER tenant moves inside the box.
	events := e.Evaluate(context.Background(), otherTenant, "dev-other", 0.5, 0.5, time.Time{})

	if len(events) != 1 {
		t.Fatalf("got %d events, want exactly 1 (its own tenant's fence): %v", len(events), events)
	}
	if events[0].FenceID != "other-fence" {
		t.Errorf("the device fired fence %q, which belongs to another tenant. "+
			"That raises somebody else's escalation chain, which since MESHSAT-1115 "+
			"sends a real SMS to a real person.", events[0].FenceID)
	}
	if events[0].TenantID != otherTenant {
		t.Errorf("the event names tenant %q, want %q", events[0].TenantID, otherTenant)
	}
}

// An unattributable position must fire nothing. "Unknown tenant" cannot mean
// "everybody's fences".
func TestAPositionWithNoTenantFiresNothing(t *testing.T) {
	e := NewEngine()
	e.AddFence(fenceIn(testTenant, "victim-fence"))
	// A fence that itself has no tenant must not become a catch-all either.
	e.AddFence(fenceIn("", "orphan-fence"))

	if events := e.Evaluate(context.Background(), "", "dev-1", 0.5, 0.5, time.Time{}); len(events) != 0 {
		t.Errorf("a tenant-less position fired %d fence events: %v", len(events), events)
	}
}

func TestListingShowsOnlyTheTenantsOwnFences(t *testing.T) {
	e := NewEngine()
	e.AddFence(fenceIn(testTenant, "victim-fence"))
	e.AddFence(fenceIn(otherTenant, "other-fence"))

	got := e.ListFences(otherTenant)
	if len(got) != 1 || got[0].ID != "other-fence" {
		t.Fatalf("tenant sees %v, want only its own fence. The listing used to hand out "+
			"every tenant's polygons and escalation chain ids.", got)
	}
}

func TestATenantCannotDeleteAnothersFence(t *testing.T) {
	e := NewEngine()
	e.AddFence(fenceIn(testTenant, "shared-id"))

	if e.RemoveFence(otherTenant, "shared-id") {
		t.Error("RemoveFence reported removing a fence belonging to another tenant")
	}
	if len(e.ListFences(testTenant)) != 1 {
		t.Error("another tenant deleted this tenant's fence")
	}
	if !e.RemoveFence(testTenant, "shared-id") {
		t.Error("the owning tenant could not remove its own fence")
	}
}

// Two tenants may legitimately use the same fence id, and one must not
// overwrite the other's polygon.
func TestTwoTenantsMayUseTheSameFenceID(t *testing.T) {
	e := NewEngine()
	mine := fenceIn(testTenant, "perimeter")
	theirs := fenceIn(otherTenant, "perimeter")
	theirs.Polygon = []Point{{Lat: 10, Lon: 10}, {Lat: 10, Lon: 11}, {Lat: 11, Lon: 11}}
	e.AddFence(mine)
	e.AddFence(theirs)

	a, b := e.ListFences(testTenant), e.ListFences(otherTenant)
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("fences: %v / %v, want one each -- one tenant overwrote the other's", a, b)
	}
	if len(b[0].Polygon) != 3 {
		t.Errorf("the second tenant's polygon has %d vertices, want 3: its fence was "+
			"replaced by the first tenant's", len(b[0].Polygon))
	}
}

// Crossing state is per tenant as well as per device: two tenants' devices with
// the same id must not share a "was inside" flag, or one tenant's movement
// suppresses the other's enter event.
func TestCrossingStateIsPerTenant(t *testing.T) {
	e := NewEngine()
	e.AddFence(fenceIn(testTenant, "f"))
	e.AddFence(fenceIn(otherTenant, "f"))
	const shared = "300434067943980"

	if got := e.Evaluate(context.Background(), otherTenant, shared, 0.5, 0.5, time.Time{}); len(got) != 1 {
		t.Fatalf("the other tenant's entry produced %d events, want 1", len(got))
	}
	// The same device id under the victim tenant has not entered anything yet,
	// so this must still read as an entry.
	got := e.Evaluate(context.Background(), testTenant, shared, 0.5, 0.5, time.Time{})
	if len(got) != 1 || got[0].EventType != "enter" {
		t.Errorf("got %v; the other tenant's crossing state suppressed this tenant's "+
			"enter event for the same device id", got)
	}
}

func TestForgetTenantDropsTheFencesAndTheState(t *testing.T) {
	e := NewEngine()
	e.AddFence(fenceIn(testTenant, "stays"))
	e.AddFence(fenceIn(otherTenant, "goes"))
	e.Evaluate(context.Background(), otherTenant, "dev-other", 0.5, 0.5, time.Time{})

	e.ForgetTenant(otherTenant)

	if len(e.ListFences(otherTenant)) != 0 {
		t.Error("a purged tenant's fences are still live")
	}
	if len(e.ListFences(testTenant)) != 1 {
		t.Error("ForgetTenant removed another tenant's fence")
	}
	// Re-adding and re-evaluating must read as a fresh entry, not as "already
	// inside" left over from before the purge.
	e.AddFence(fenceIn(otherTenant, "goes"))
	got := e.Evaluate(context.Background(), otherTenant, "dev-other", 0.5, 0.5, time.Time{})
	if len(got) != 1 || got[0].EventType != "enter" {
		t.Errorf("got %v; crossing state survived the purge", got)
	}
}

func TestTheFenceKeyCannotBeForgedByAColon(t *testing.T) {
	if scope("a", "b:c") == scope("a:b", "c") {
		t.Error("scope() collides: a fence id containing the separator lets one tenant " +
			"address another tenant's fence")
	}
}

// The engine must satisfy tenancy.TenantForgetter. Asserted against a local copy
// of the one method, because importing internal/tenancy here would invert the
// dependency.
var _ interface{ ForgetTenant(string) } = (*Engine)(nil)

// Evaluate used to call its event handlers while still holding e.mu -- the
// `defer e.mu.Unlock()` outlived the loop, under a comment that said "outside
// the critical section". Nothing caught it because nothing calls OnEvent
// (MESHSAT-1119), so the handler list is always empty in production today.
//
// Two things go wrong the moment somebody wires the first handler up. A handler
// that touches the engine deadlocks, because sync.Mutex is not reentrant. And a
// handler that does I/O -- a fence's escalation chain sends an SMS -- holds
// every other tenant's evaluation behind its network call.
//
// This test fails by HANGING rather than by reporting, which is what a deadlock
// does; the timeout is what turns it back into a failure.
func TestAHandlerMayCallBackIntoTheEngine(t *testing.T) {
	e := NewEngine()
	e.AddFence(fenceIn(testTenant, "f"))

	var reentered bool
	e.OnEvent(func(_ context.Context, ev FenceEvent) {
		// Exactly what a real handler would plausibly do: look up the fence it
		// was told about.
		for _, f := range e.ListFences(ev.TenantID) {
			if f.ID == ev.FenceID {
				reentered = true
			}
		}
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5, time.Time{})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Evaluate did not return within 5s: a handler that reads the engine " +
			"deadlocked against the lock Evaluate was still holding")
	}
	if !reentered {
		t.Error("the handler ran but could not see the fence it was notified about")
	}
}
