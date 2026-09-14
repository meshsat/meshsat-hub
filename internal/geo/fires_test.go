package geo

import (
	"context"
	"sync"
	"testing"
	"time"
)

// MESHSAT-1119. The engine had Evaluate and OnEvent and NOTHING in the Hub
// called either: a fence could be created, listed and deleted, no position was
// ever compared against one, no FenceEvent was ever emitted, and Fence.ChainID
// -- which the create form asks for by name -- was read by nothing.
//
// These tests are about the event a crossing produces, because that event is
// the entire product: it is what reaches the escalation chain and makes a phone
// ring.

type collector struct {
	mu     sync.Mutex
	events []FenceEvent
}

func (c *collector) handle(_ context.Context, ev FenceEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
}

func (c *collector) all() []FenceEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]FenceEvent(nil), c.events...)
}

// The event has to carry the chain. Without it the handler would have to look
// the fence up again -- after Evaluate released its lock, by which time the
// fence may have been edited or deleted, and a crossing that already happened
// must still page whoever was on call for it.
func TestACrossingCarriesTheEscalationChain(t *testing.T) {
	e := NewEngine()
	c := &collector{}
	e.OnEvent(c.handle)
	e.AddFence(fenceIn(testTenant, "perimeter"))

	e.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5, time.Time{})

	got := c.all()
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if got[0].ChainID != "chain-"+testTenant {
		t.Errorf("the event carries chain %q, want %q. Without it nothing can raise the "+
			"alert the customer configured, which is what MESHSAT-1119 was.",
			got[0].ChainID, "chain-"+testTenant)
	}
	if got[0].TenantID != testTenant || got[0].DeviceIMEI != "dev-a" || got[0].EventType != "enter" {
		t.Errorf("event = %+v", got[0])
	}
	if got[0].Lat != 0.5 || got[0].Lon != 0.5 {
		t.Errorf("the event does not carry where it happened: %+v", got[0])
	}
}

// A crossing fires ONCE. A device reporting its position every minute inside a
// fence must not page somebody every minute.
func TestACrossingFiresOncePerTransition(t *testing.T) {
	e := NewEngine()
	c := &collector{}
	e.OnEvent(c.handle)
	e.AddFence(fenceIn(testTenant, "perimeter"))

	for i := 0; i < 5; i++ {
		e.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5, time.Time{})
	}
	if n := len(c.all()); n != 1 {
		t.Fatalf("five positions inside the fence produced %d events, want 1. "+
			"An escalation chain sends a real SMS to a real person.", n)
	}

	// Leaving and re-entering is two more transitions.
	e.Evaluate(context.Background(), testTenant, "dev-a", 50, 50, time.Time{})
	e.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5, time.Time{})
	got := c.all()
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3 (enter, exit, enter): %v", len(got), got)
	}
	if got[1].EventType != "exit" || got[2].EventType != "enter" {
		t.Errorf("transitions = %s, %s; want exit, enter", got[1].EventType, got[2].EventType)
	}
}

// A fence set to one direction only must not fire on the other.
func TestTriggerModeIsHonoured(t *testing.T) {
	for _, tc := range []struct {
		mode TriggerMode
		want []string
	}{
		{TriggerEnter, []string{"enter"}},
		{TriggerExit, []string{"exit"}},
		{TriggerBoth, []string{"enter", "exit"}},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			e := NewEngine()
			c := &collector{}
			e.OnEvent(c.handle)
			f := fenceIn(testTenant, "f")
			f.Trigger = tc.mode
			e.AddFence(f)

			e.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5, time.Time{}) // enter
			e.Evaluate(context.Background(), testTenant, "dev-a", 50, 50, time.Time{})   // exit

			var kinds []string
			for _, ev := range c.all() {
				kinds = append(kinds, ev.EventType)
			}
			if len(kinds) != len(tc.want) {
				t.Fatalf("%s produced %v, want %v", tc.mode, kinds, tc.want)
			}
			for i := range kinds {
				if kinds[i] != tc.want[i] {
					t.Fatalf("%s produced %v, want %v", tc.mode, kinds, tc.want)
				}
			}
		})
	}
}

// A disabled fence pages nobody.
func TestADisabledFenceFiresNothing(t *testing.T) {
	e := NewEngine()
	c := &collector{}
	e.OnEvent(c.handle)
	f := fenceIn(testTenant, "off")
	f.Enabled = false
	e.AddFence(f)

	e.Evaluate(context.Background(), testTenant, "dev-a", 0.5, 0.5, time.Time{})
	if n := len(c.all()); n != 0 {
		t.Errorf("a disabled fence produced %d events", n)
	}
}

// And the isolation property, stated at the level that matters: one tenant's
// device must never page another tenant's on-call rota.
func TestADeviceNeverRaisesAnotherTenantsChain(t *testing.T) {
	e := NewEngine()
	c := &collector{}
	e.OnEvent(c.handle)
	e.AddFence(fenceIn(testTenant, "victim-fence"))
	e.AddFence(fenceIn(otherTenant, "own-fence"))

	e.Evaluate(context.Background(), otherTenant, "dev-other", 0.5, 0.5, time.Time{})

	for _, ev := range c.all() {
		if ev.ChainID == "chain-"+testTenant {
			t.Fatalf("a device raised another tenant's escalation chain (%s). "+
				"Since MESHSAT-1115 that sends a real SMS to a real person.", ev.ChainID)
		}
	}
}
