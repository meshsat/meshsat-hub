package storetest

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/escalation"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// The r16 review's C10 lifecycle findings, on every driver (MESHSAT-1584).

// testSOSRunAlertPending (C10-R2): a run's state change owes its alert work in the same row,
// cleared only for the rev the work was done for, and listed across tenants until cleared.
func testSOSRunAlertPending(t *testing.T, s store.Store) {
	ctx := context.Background()
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	ev := func(tenant, kind, sosID string, sec int) *store.SOSRunEvent {
		return &store.SOSRunEvent{TenantID: tenant, Origin: "bridge:kit-a", SOSID: sosID, BridgeID: "kit-a",
			RunStartMs: 1792330000123, Kind: kind, Bearer: "mqtt", At: at.Add(time.Duration(sec) * time.Second)}
	}
	a, err := s.ApplySOSRunEvent(ctx, ev("t-pend-a", store.SOSEventStart, "01a14f31-66fb-7a7b-bc7d-7e7f80818283", 0))
	if err != nil || !a.Created || !a.Run.AlertPending {
		t.Fatalf("a new start owes no alert work: %+v %v", a.Run, err)
	}
	b, err := s.ApplySOSRunEvent(ctx, ev("t-pend-b", store.SOSEventCancel, "01a14f31-66fb-7a7b-bc7d-7e7f80818284", 1))
	if err != nil || !b.Run.AlertPending {
		t.Fatalf("a cancel before its start owes no alert work: %+v %v", b.Run, err)
	}
	listed := func() map[string]int {
		runs, err := s.ListSOSRunsAlertPending(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int{}
		for _, r := range runs {
			out[r.TenantID+"/"+r.SOSID] = r.Rev
		}
		return out
	}
	if l := listed(); len(l) != 2 {
		t.Fatalf("listed %v, want both tenants' runs", l)
	}
	// A clear for an older rev keeps it owed.
	dup, err := s.ApplySOSRunEvent(ctx, ev("t-pend-a", store.SOSEventStart, "01a14f31-66fb-7a7b-bc7d-7e7f80818283", 2))
	if err != nil || dup.Run.Rev != 2 || !dup.Run.AlertPending {
		t.Fatalf("a duplicate start: %+v %v", dup.Run, err)
	}
	if ok, err := s.ClearSOSRunAlertPending(ctx, "t-pend-a", "bridge:kit-a", "01a14f31-66fb-7a7b-bc7d-7e7f80818283", 1); err != nil || ok {
		t.Fatalf("cleared for a stale rev: %v %v", ok, err)
	}
	if ok, err := s.ClearSOSRunAlertPending(ctx, "t-pend-a", "bridge:kit-a", "01a14f31-66fb-7a7b-bc7d-7e7f80818283", 2); err != nil || !ok {
		t.Fatalf("not cleared for its rev: %v %v", ok, err)
	}
	if l := listed(); len(l) != 1 {
		t.Fatalf("after the clear: %v", l)
	}
	// A duplicate does not make it owed again; the cancel that ends it does.
	if d, _ := s.ApplySOSRunEvent(ctx, ev("t-pend-a", store.SOSEventStart, "01a14f31-66fb-7a7b-bc7d-7e7f80818283", 3)); d.Run.AlertPending {
		t.Fatal("a duplicate start made the alert work owed again")
	}
	c, err := s.ApplySOSRunEvent(ctx, ev("t-pend-a", store.SOSEventCancel, "01a14f31-66fb-7a7b-bc7d-7e7f80818283", 4))
	if err != nil || !c.Ended || !c.Run.AlertPending {
		t.Fatalf("the cancel that ends it owes no alert work: %+v %v", c.Run, err)
	}
	got, err := s.GetSOSRun(ctx, "t-pend-a", "bridge:kit-a", "01a14f31-66fb-7a7b-bc7d-7e7f80818283")
	if err != nil || !got.AlertPending {
		t.Fatalf("not stored owed: %+v %v", got, err)
	}
}

// barrier is a notifier that runs a function during its first notification.
type barrier struct {
	calls  atomic.Int32
	during func()
}

func (b *barrier) Notify(context.Context, []string, string, string) error {
	if b.calls.Add(1) == 1 && b.during != nil {
		b.during()
	}
	return nil
}

func escalationFixture(t *testing.T, s store.Store, tenant, alertID string, at time.Time) *store.Alert {
	t.Helper()
	ctx := context.Background()
	chain := &store.EscalationChain{ID: "chain-" + alertID, Name: "on call",
		Tiers: []store.EscalationTier{{Name: "first", Targets: []string{"fake://x"}, WaitSec: 60, MaxRetries: 3}}}
	if err := s.CreateEscalationChain(ctx, tenant, chain); err != nil {
		t.Fatal(err)
	}
	a := &store.Alert{ID: alertID, ChainID: chain.ID, DeviceIMEI: "kit-a", Type: "sos", Detail: "x",
		State: store.AlertStateTriggered, NextEscAt: at, CreatedAt: at, UpdatedAt: at}
	if err := s.CreateAlert(ctx, tenant, a); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAlert(ctx, tenant, alertID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// testEscalationStepCannotReopenACancel (C10-R1): a cancel that lands while the notifier of
// an escalation step runs stays terminal through the step's completion and every later
// tick, and is still attributed to who cancelled; one notification only.
func testEscalationStepCannotReopenACancel(t *testing.T, s store.Store) {
	ctx := context.Background()
	const tenant = "t-esc-r1"
	at := time.Now().UTC().Add(-time.Second).Truncate(time.Millisecond)
	a := escalationFixture(t, s, tenant, "alert-r1", at)
	e := escalation.New(s, nil)
	n := &barrier{during: func() {
		if ok, err := e.Cancel(ctx, tenant, "alert-r1", "bridge:kit-a"); err != nil || !ok {
			t.Errorf("cancel during the notifier: %v %v", ok, err)
		}
	}}
	e.SetNotifier(n)
	e.Step(ctx, a, at.Add(time.Second))
	got, err := s.GetAlert(ctx, tenant, "alert-r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.AlertStateCancelled || got.AckedBy != "bridge:kit-a" {
		t.Fatalf("after the step: state %q acked_by %q", got.State, got.AckedBy)
	}
	e.Step(ctx, got, at.Add(10*time.Minute))
	got, _ = s.GetAlert(ctx, tenant, "alert-r1")
	if got.State != store.AlertStateCancelled || got.AckedBy != "bridge:kit-a" || n.calls.Load() != 1 {
		t.Fatalf("after a later tick: state %q acked_by %q, %d notifications", got.State, got.AckedBy, n.calls.Load())
	}
}

// testEscalationStaleWorkerKeepsItsHandsOff (C10-R1): a worker whose lease ran out while its
// notifier hung does not overwrite the step another replica took and finished meanwhile.
func testEscalationStaleWorkerKeepsItsHandsOff(t *testing.T, s store.Store) {
	ctx := context.Background()
	const tenant = "t-esc-r1b"
	at := time.Now().UTC().Add(-time.Second).Truncate(time.Millisecond)
	a := escalationFixture(t, s, tenant, "alert-r1b", at)
	other := escalation.New(s, nil)
	other.SetNotifier(&barrier{})
	later := at.Add(10 * time.Minute) // past the stale worker's lease
	var otherNext time.Time
	stale := escalation.New(s, nil)
	stale.SetNotifier(&barrier{during: func() {
		cur, err := s.GetAlert(ctx, tenant, "alert-r1b")
		if err != nil {
			t.Error(err)
			return
		}
		other.Step(ctx, cur, later) // the lease ran out; the other replica takes the step
		after, _ := s.GetAlert(ctx, tenant, "alert-r1b")
		otherNext = after.NextEscAt
	}})
	stale.Step(ctx, a, at.Add(time.Second))
	got, err := s.GetAlert(ctx, tenant, "alert-r1b")
	if err != nil {
		t.Fatal(err)
	}
	if otherNext.IsZero() || !got.NextEscAt.Equal(otherNext) || got.Retries != 1 {
		t.Fatalf("the stale worker overwrote the other's step: next %v (the other's %v), retries %d", got.NextEscAt, otherNext, got.Retries)
	}
}
