package geo

import (
	"context"
	"testing"
	"time"
)

// MESHSAT-1119 follow-up. Making fences fire introduced a failure mode that
// could not exist while nothing fired: a device parked on a boundary -- GPS
// jitter is metres, and a vehicle at a depot gate is the obvious case --
// produces enter/exit/enter/exit, and every one of those raises an escalation
// chain and sends a real SMS.
//
// The cooldown damps the OUTPUT. The first crossing fires immediately and is
// never delayed; repeats for the same device and fence are dropped for the
// window.
//
// The alternative, dwell time, damps the INPUT by waiting for a second report
// on the new side. That was the first recommendation on the issue and it is
// wrong for this fleet: Iridium SBD positions arrive minutes apart, so it would
// delay EVERY genuine crossing alert by a full reporting interval. On a safety
// path a late alert is worse than a duplicate one. TestTheFirstCrossingIsNeverDelayed
// is the test that pins that choice.

// at builds an engine whose clock the test drives.
func at(start time.Time) (*Engine, *time.Time) {
	e := NewEngine()
	now := start
	e.now = func() time.Time { return now }
	return e, &now
}

// The property the whole design turns on.
func TestTheFirstCrossingIsNeverDelayed(t *testing.T) {
	e, _ := at(time.Unix(1_700_000_000, 0))
	e.SetCooldown(5 * time.Minute)
	c := &collector{}
	e.OnEvent(c.handle)
	e.AddFence(fenceIn(testTenant, "perimeter"))

	got := e.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5, time.Time{})

	if len(got) != 1 {
		t.Fatalf("the first crossing produced %d events, want 1. A cooldown must never "+
			"hold back the FIRST alert -- that is the difference between this and dwell "+
			"time, and the reason this shape was chosen.", len(got))
	}
	if len(c.all()) != 1 {
		t.Errorf("the handler saw %d events, want 1", len(c.all()))
	}
}

func TestRepeatCrossingsAreSuppressedWithinTheWindow(t *testing.T) {
	e, now := at(time.Unix(1_700_000_000, 0))
	e.SetCooldown(5 * time.Minute)
	c := &collector{}
	e.OnEvent(c.handle)
	e.AddFence(fenceIn(testTenant, "perimeter"))

	// A device jittering across the line every ten seconds for two minutes.
	inside := true
	for i := 0; i < 12; i++ {
		if inside {
			e.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5, time.Time{})
		} else {
			e.Evaluate(context.Background(), testTenant, "dev-a", 50, 50, time.Time{})
		}
		inside = !inside
		*now = now.Add(10 * time.Second)
	}

	if n := len(c.all()); n != 1 {
		t.Errorf("twelve crossings in two minutes produced %d pages, want 1. Each one is "+
			"a real SMS to a real person.", n)
	}
}

// And once the window passes, the fence works again -- a cooldown must not be
// a permanent mute.
func TestTheFenceFiresAgainAfterTheWindow(t *testing.T) {
	e, now := at(time.Unix(1_700_000_000, 0))
	e.SetCooldown(5 * time.Minute)
	c := &collector{}
	e.OnEvent(c.handle)
	e.AddFence(fenceIn(testTenant, "perimeter"))

	e.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5, time.Time{}) // enter, fires
	*now = now.Add(30 * time.Second)
	e.Evaluate(context.Background(), testTenant, "dev-a", 50, 50, time.Time{}) // exit, suppressed
	if n := len(c.all()); n != 1 {
		t.Fatalf("got %d events inside the window, want 1", n)
	}

	*now = now.Add(6 * time.Minute)
	e.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5, time.Time{}) // enter, fires again
	if n := len(c.all()); n != 2 {
		t.Errorf("got %d events after the window expired, want 2 -- a cooldown is not a mute", n)
	}
}

// The sharp one. Suppressing the EVENT must not stop the crossing STATE being
// updated, or the inside/outside flag goes stale and a later genuine transition
// is missed entirely -- the fence would go quiet for good.
func TestSuppressingAnEventStillTracksWhereTheDeviceIs(t *testing.T) {
	e, now := at(time.Unix(1_700_000_000, 0))
	e.SetCooldown(5 * time.Minute)
	c := &collector{}
	e.OnEvent(c.handle)
	e.AddFence(fenceIn(testTenant, "perimeter"))

	e.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5, time.Time{}) // enter, fires
	*now = now.Add(10 * time.Second)
	e.Evaluate(context.Background(), testTenant, "dev-a", 50, 50, time.Time{}) // exit, suppressed

	// Six minutes later the device is still OUTSIDE. That is not a transition,
	// so nothing should fire however long the cooldown has been over.
	*now = now.Add(6 * time.Minute)
	e.Evaluate(context.Background(), testTenant, "dev-a", 50, 50, time.Time{})
	if n := len(c.all()); n != 1 {
		t.Fatalf("a device that has not moved produced %d events, want 1. The suppressed "+
			"exit did not update the state, so the engine still thinks it is inside.", n)
	}

	// Now it genuinely re-enters, and that must fire.
	e.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5, time.Time{})
	if n := len(c.all()); n != 2 {
		t.Errorf("a genuine re-entry produced %d events total, want 2", n)
	}
}

// A fence may override the platform default.
func TestAFenceMayChooseItsOwnCooldown(t *testing.T) {
	e, now := at(time.Unix(1_700_000_000, 0))
	e.SetCooldown(time.Hour) // a long platform default
	c := &collector{}
	e.OnEvent(c.handle)
	f := fenceIn(testTenant, "quick")
	f.CooldownSec = 60
	e.AddFence(f)

	e.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5, time.Time{})
	*now = now.Add(90 * time.Second)
	e.Evaluate(context.Background(), testTenant, "dev-a", 50, 50, time.Time{})

	if n := len(c.all()); n != 2 {
		t.Errorf("got %d events, want 2: the fence's own 60s cooldown should have expired "+
			"even though the platform default is an hour", n)
	}
}

// The cooldown is per device AND per fence: one device going quiet must not
// silence another's crossing of the same fence.
func TestTheCooldownIsPerDeviceAndPerFence(t *testing.T) {
	e, _ := at(time.Unix(1_700_000_000, 0))
	e.SetCooldown(5 * time.Minute)
	c := &collector{}
	e.OnEvent(c.handle)
	e.AddFence(fenceIn(testTenant, "one"))
	e.AddFence(fenceIn(testTenant, "two"))

	// dev-a crosses both fences (the box is the same, so both fire), then dev-b
	// crosses too. Nothing here shares a cooldown.
	e.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5, time.Time{})
	e.Evaluate(context.Background(), testTenant, "dev-b", 0.5, 0.5, time.Time{})

	if n := len(c.all()); n != 4 {
		t.Errorf("got %d events, want 4 (two devices x two fences). A cooldown keyed too "+
			"coarsely silences crossings that have nothing to do with each other.", n)
	}
}

// Zero means no suppression at all, which is what an engine gets before
// SetCooldown is called -- so a Hub built without the config behaves exactly as
// it did before this was added.
func TestNoCooldownConfiguredMeansEveryCrossingFires(t *testing.T) {
	e, now := at(time.Unix(1_700_000_000, 0))
	c := &collector{}
	e.OnEvent(c.handle)
	e.AddFence(fenceIn(testTenant, "perimeter"))

	for i := 0; i < 4; i++ {
		if i%2 == 0 {
			e.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5, time.Time{})
		} else {
			e.Evaluate(context.Background(), testTenant, "dev-a", 50, 50, time.Time{})
		}
		*now = now.Add(time.Second)
	}
	if n := len(c.all()); n != 4 {
		t.Errorf("got %d events with no cooldown set, want 4", n)
	}
}

// Deleting a fence forgets its cooldown stamps, so re-creating it is a clean
// slate rather than inheriting a mute from the fence that used to have that id.
func TestRemovingAFenceForgetsItsCooldown(t *testing.T) {
	e, _ := at(time.Unix(1_700_000_000, 0))
	e.SetCooldown(time.Hour)
	c := &collector{}
	e.OnEvent(c.handle)
	e.AddFence(fenceIn(testTenant, "perimeter"))

	e.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5, time.Time{})
	e.RemoveFence(testTenant, "perimeter")
	e.AddFence(fenceIn(testTenant, "perimeter"))
	e.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5, time.Time{})

	if n := len(c.all()); n != 2 {
		t.Errorf("got %d events, want 2: a re-created fence inherited the deleted one's "+
			"cooldown and stayed silent", n)
	}
}
