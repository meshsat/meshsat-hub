package escalation

import (
	"context"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// MESHSAT-1115. processAlert used to log "chain not found" and RETURN, leaving
// NextEscAt in the past -- so the alert stayed due and the engine re-processed
// it every 10 seconds, for ever, at Error level, for an alert it could never
// deliver.
//
// This is reachable in ordinary use rather than only from corruption: there is
// no UpdateEscalationChain at any layer, so editing a chain means delete and
// recreate, which orphans every alert already pointing at the old id.
func TestAnAlertWhoseChainWasDeletedIsClosedRatherThanLooping(t *testing.T) {
	s := newTestStore(t)
	notifier := &recordingNotifier{}
	e := New(s, notifier)
	ctx := context.Background()

	chain := &store.EscalationChain{
		Name:  "about to be deleted",
		Tiers: []store.EscalationTier{{Name: "t1", Targets: []string{"+31600000000"}, MaxRetries: 1}},
	}
	if err := s.CreateEscalationChain(ctx, "default", chain); err != nil {
		t.Fatalf("chain: %v", err)
	}
	alert := &store.Alert{ChainID: chain.ID, DeviceIMEI: "dev1", Type: "sos", Detail: "help"}
	if err := e.Trigger(ctx, "default", alert); err != nil {
		t.Fatalf("trigger: %v", err)
	}

	// The chain goes away underneath the live alert, exactly as a delete and
	// recreate would leave it.
	if err := s.DeleteEscalationChain(ctx, "default", chain.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	now := time.Now().UTC()
	e.processAlert(ctx, alert, now)

	got, err := s.GetAlert(ctx, "", alert.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != store.AlertStateExhausted {
		t.Fatalf("state %q, want %q -- an alert left active with a missing chain is "+
			"re-processed every interval for ever", got.State, store.AlertStateExhausted)
	}

	// And it must genuinely leave the active set, which is what stops the loop.
	active, err := s.ListAlerts(ctx, "", true, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, a := range active {
		if a.ID == alert.ID {
			t.Error("the orphaned alert is still in the active set, so the engine will keep picking it up")
		}
	}
	if notifier.callCount() != 0 {
		t.Errorf("notified %d times for an alert with no chain", notifier.callCount())
	}
}

// The tier walk reads backwards and is easy to get wrong, so pin it: a tier's
// OWN WaitSec is the delay before that tier fires, applied when advancing into
// it. Tier 0's WaitSec is never a delay -- tier 0 fires on the first tick.
func TestATiersWaitIsTheDelayBeforeThatTierFires(t *testing.T) {
	s := newTestStore(t)
	e := New(s, &recordingNotifier{})
	ctx := context.Background()

	chain := &store.EscalationChain{
		Name: "paced",
		Tiers: []store.EscalationTier{
			// Tier 0's 999 must be ignored as a delay; it fires immediately.
			{Name: "first", Targets: []string{"+31600000001"}, WaitSec: 999, MaxRetries: 1},
			{Name: "second", Targets: []string{"+31600000002"}, WaitSec: 300, MaxRetries: 1},
		},
	}
	if err := s.CreateEscalationChain(ctx, "default", chain); err != nil {
		t.Fatalf("chain: %v", err)
	}
	alert := &store.Alert{ChainID: chain.ID, DeviceIMEI: "dev1", Type: "sos"}
	if err := e.Trigger(ctx, "default", alert); err != nil {
		t.Fatalf("trigger: %v", err)
	}

	now := time.Now().UTC()
	e.processAlert(ctx, alert, now)

	got, _ := s.GetAlert(ctx, "", alert.ID)
	if got.CurrentTier != 1 {
		t.Fatalf("current tier %d, want 1", got.CurrentTier)
	}
	// The gap comes from tier 1's WaitSec (300), not tier 0's (999).
	gap := got.NextEscAt.Sub(now)
	if gap < 290*time.Second || gap > 310*time.Second {
		t.Errorf("next escalation in %v, want ~300s taken from the tier being entered, "+
			"not %ds from the tier being left", gap, chain.Tiers[0].WaitSec)
	}
}
