package escalation

import (
	"context"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// The r16 review's reproduction of C10-R1, as it gave it (formatted): a cancel during the
// notifier must stay terminal and keep its attribution; one notification only.

type reviewNotify func(context.Context, []string, string, string) error

func (f reviewNotify) Notify(c context.Context, x []string, s, b string) error { return f(c, x, s, b) }

func TestReviewR16CancelDuringNotifyStaysTerminal(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	chain := &store.EscalationChain{ID: "review-chain", Name: "review", Tiers: []store.EscalationTier{{Name: "first", Targets: []string{"local-fake"}, WaitSec: 1, MaxRetries: 3}}}
	if err := st.CreateEscalationChain(ctx, "default", chain); err != nil {
		t.Fatal(err)
	}
	e := New(st, nil)
	calls := 0
	e.SetNotifier(reviewNotify(func(c context.Context, _ []string, _, _ string) error {
		calls++
		if calls == 1 {
			if ok, err := e.Cancel(c, "default", "review-alert", "bridge:review"); err != nil || !ok {
				t.Fatalf("cancel: %v %v", ok, err)
			}
		}
		return nil
	}))
	if err := e.Trigger(ctx, "default", &store.Alert{ID: "review-alert", ChainID: chain.ID, Type: "sos"}); err != nil {
		t.Fatal(err)
	}
	a, err := st.GetAlert(ctx, "default", "review-alert")
	if err != nil {
		t.Fatal(err)
	}
	e.processAlert(ctx, a, time.Now().UTC())
	after, err := st.GetAlert(ctx, "default", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after cancellation during notification: state=%s acked_by=%q", after.State, after.AckedBy)
	if after.State != store.AlertStateCancelled {
		e.processAlert(ctx, after, time.Now().UTC().Add(time.Minute))
		t.Fatalf("cancel reopened: state=%s; notifications=%d", after.State, calls)
	}
	e.processAlert(ctx, after, time.Now().UTC().Add(time.Minute))
	final, _ := st.GetAlert(ctx, "default", a.ID)
	if final.State != store.AlertStateCancelled || final.AckedBy != "bridge:review" || calls != 1 {
		t.Fatalf("a later tick: state=%s acked_by=%q notifications=%d", final.State, final.AckedBy, calls)
	}
}

// The early exits of a step (its chain deleted, every tier used) do not overwrite an alert
// cancelled after it was listed (C10-R1).
func TestAnEarlyExitLeavesACancelledAlertCancelled(t *testing.T) {
	for _, exit := range []string{"chain missing", "tiers exhausted"} {
		st := newTestStore(t)
		ctx := context.Background()
		chain := &store.EscalationChain{ID: "c-exit", Name: "x", Tiers: []store.EscalationTier{{Name: "only", Targets: []string{"x"}, WaitSec: 1, MaxRetries: 1}}}
		if exit == "tiers exhausted" {
			if err := st.CreateEscalationChain(ctx, "default", chain); err != nil {
				t.Fatal(err)
			}
		}
		e := New(st, nil)
		if err := e.Trigger(ctx, "default", &store.Alert{ID: "a-exit", ChainID: chain.ID, Type: "sos"}); err != nil {
			t.Fatal(err)
		}
		listed, _ := st.GetAlert(ctx, "default", "a-exit") // what the loop listed
		if exit == "tiers exhausted" {
			listed.CurrentTier = 1
		}
		if ok, err := e.Cancel(ctx, "default", "a-exit", "user:u1"); err != nil || !ok {
			t.Fatalf("%s: cancel %v %v", exit, ok, err)
		}
		e.processAlert(ctx, listed, time.Now().UTC().Add(time.Minute))
		got, _ := st.GetAlert(ctx, "default", "a-exit")
		if got.State != store.AlertStateCancelled || got.AckedBy != "user:u1" {
			t.Errorf("%s: the stale exit wrote state %s acked_by %q", exit, got.State, got.AckedBy)
		}
	}
}
