package escalation

import (
	"context"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// TriggerOnce raises an alert under its own id once: a second raise of the same id is not an
// error and changes nothing (an SOS run pages once, HeMB-NG C10).
func TestTriggerOnceRaisesAnIDOnce(t *testing.T) {
	st := newTestStore(t)
	e := New(st, nil)
	ctx := context.Background()
	created, err := e.TriggerOnce(ctx, "default", &store.Alert{ID: "sos-1", ChainID: "c1", Type: "sos", Detail: "first"})
	if err != nil || !created {
		t.Fatalf("first: %v %v", created, err)
	}
	created, err = e.TriggerOnce(ctx, "default", &store.Alert{ID: "sos-1", ChainID: "c1", Type: "sos", Detail: "second"})
	if err != nil || created {
		t.Fatalf("second: %v %v", created, err)
	}
	a, _ := st.GetAlert(ctx, "default", "sos-1")
	if a == nil || a.Detail != "first" {
		t.Errorf("alert %+v", a)
	}
	if _, err := e.TriggerOnce(ctx, "default", &store.Alert{ChainID: "c1"}); err == nil {
		t.Error("an alert without an id was raised")
	}
}

// Cancel closes an alert for good, records who, and an escalation step that read it before
// cannot reopen it; cancelling an unknown or cancelled alert is no change.
func TestCancelClosesAnAlertForGood(t *testing.T) {
	st := newTestStore(t)
	e := New(st, nil)
	ctx := context.Background()
	if _, err := e.TriggerOnce(ctx, "default", &store.Alert{ID: "sos-2", ChainID: "c1", Type: "sos"}); err != nil {
		t.Fatal(err)
	}
	read, _ := st.GetAlert(ctx, "default", "sos-2") // an escalation step read it
	done, err := e.Cancel(ctx, "default", "sos-2", "bridge:kit-a")
	if err != nil || !done {
		t.Fatalf("cancel: %v %v", done, err)
	}
	a, _ := st.GetAlert(ctx, "default", "sos-2")
	if a.State != store.AlertStateCancelled || a.AckedBy != "bridge:kit-a" || a.AckedAt.IsZero() {
		t.Errorf("alert %+v", a)
	}
	expected := read.NextEscAt
	read.State, read.CurrentTier, read.NextEscAt = store.AlertStateEscalating, 1, time.Now().Add(time.Minute)
	if ok, _ := st.AdvanceAlert(ctx, "default", read, expected); ok {
		t.Error("an escalation step reopened the cancelled alert")
	}
	if done, err := e.Cancel(ctx, "default", "sos-2", "user:u1"); err != nil || done {
		t.Errorf("second cancel: %v %v", done, err)
	}
	if done, err := e.Cancel(ctx, "default", "nope", "user:u1"); err != nil || done {
		t.Errorf("unknown alert: %v %v", done, err)
	}
	// The loop no longer picks it up.
	active, _ := st.ListAlerts(ctx, "default", true, 10)
	for _, x := range active {
		if x.ID == "sos-2" {
			t.Error("a cancelled alert is listed active")
		}
	}
}
