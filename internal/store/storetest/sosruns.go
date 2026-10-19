package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// testSOSRuns pins the SOS run store (HeMB-NG stage 2, C10, MESHSAT-1584) in both drivers:
// a start makes an active run and repeats only add their bearer; a cancel ends it for good,
// also when it arrives before its start; nothing reopens a cancelled run; runs are the
// tenant's own.
func testSOSRuns(t *testing.T, s store.Store) {
	ctx := context.Background()
	at := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	const tenant, other = "t-sos", "t-other"
	ev := func(id, kind, bearer string) *store.SOSRunEvent {
		at = at.Add(time.Second)
		return &store.SOSRunEvent{TenantID: tenant, Origin: "bridge:kit-a", SOSID: id, BridgeID: "kit-a",
			RunStartMs: 1792330000123, Kind: kind, Bearer: bearer, Text: "Injured, need help", Lat: 52.16, Lon: 4.49,
			HasPosition: true, By: "bridge:kit-a", Reason: 0, At: at}
	}
	const id = "0199b7c4-f53b-7a7b-bc7d-7e7f80818283"

	a, err := s.ApplySOSRunEvent(ctx, ev(id, store.SOSEventStart, "imt"))
	if err != nil {
		t.Fatal(err)
	}
	if !a.Created || a.Ended || a.Run.State != store.SOSRunActive || a.Run.Rev != 1 || a.Run.Bearers != "imt" ||
		a.Run.StartedAt == nil || a.Run.CancelledAt != nil || a.Run.CancelReason != -1 {
		t.Fatalf("first start: %+v", a)
	}
	a, err = s.ApplySOSRunEvent(ctx, ev(id, store.SOSEventStart, "sms"))
	if err != nil || a.Created || a.Ended || a.Run.State != store.SOSRunActive || a.Run.Rev != 2 || a.Run.Bearers != "imt,sms" {
		t.Fatalf("the same start over another bearer: %+v %v", a, err)
	}
	a, err = s.ApplySOSRunEvent(ctx, ev(id, store.SOSEventStart, "imt"))
	if err != nil || a.Run.Rev != 3 || a.Run.Bearers != "imt,sms" {
		t.Fatalf("the same start again: %+v %v", a, err)
	}
	cancel := ev(id, store.SOSEventCancel, "mqtt")
	cancel.Reason = 1
	a, err = s.ApplySOSRunEvent(ctx, cancel)
	if err != nil || !a.Ended || a.Run.State != store.SOSRunCancelled || a.Run.CancelledAt == nil ||
		a.Run.CancelledBy != "bridge:kit-a" || a.Run.CancelReason != 1 || a.Run.Bearers != "imt,sms,mqtt" {
		t.Fatalf("the cancel: %+v %v", a, err)
	}
	a, err = s.ApplySOSRunEvent(ctx, ev(id, store.SOSEventStart, "sbd"))
	if err != nil || a.Ended || a.Run.State != store.SOSRunCancelled || a.Run.Rev != 5 {
		t.Fatalf("a start after the cancel reopened the run: %+v %v", a, err)
	}
	a, err = s.ApplySOSRunEvent(ctx, ev(id, store.SOSEventCancel, "sms"))
	if err != nil || a.Ended || a.Run.State != store.SOSRunCancelled || a.Run.CancelReason != 1 {
		t.Fatalf("a second cancel changed the first: %+v %v", a, err)
	}

	// A cancel before its start is terminal; the start that follows fills in its content.
	const early = "0199b7c4-f53b-7000-8000-000000000001"
	a, err = s.ApplySOSRunEvent(ctx, ev(early, store.SOSEventCancel, "sms"))
	if err != nil || !a.Created || a.Ended || a.Run.State != store.SOSRunCancelled || a.Run.StartedAt != nil {
		t.Fatalf("cancel before start: %+v %v", a, err)
	}
	a, err = s.ApplySOSRunEvent(ctx, ev(early, store.SOSEventStart, "imt"))
	if err != nil || a.Created || a.Run.State != store.SOSRunCancelled || a.Run.StartedAt == nil || a.Run.Text == "" {
		t.Fatalf("start after an early cancel: %+v %v", a, err)
	}

	got, err := s.GetSOSRun(ctx, tenant, "bridge:kit-a", id)
	if err != nil || got.State != store.SOSRunCancelled || !got.HasPosition || got.Lat != 52.16 {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := s.GetSOSRun(ctx, other, "bridge:kit-a", id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another tenant read the run: %v", err)
	}
	if err := s.SetSOSRunAlert(ctx, tenant, "bridge:kit-a", id, "sos-abc"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSOSRunAlert(ctx, other, "bridge:kit-a", id, "sos-abc"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another tenant set the run's alert: %v", err)
	}
	found, err := s.FindSOSRuns(ctx, tenant, id)
	if err != nil || len(found) != 1 || found[0].AlertID != "sos-abc" {
		t.Fatalf("find: %+v %v", found, err)
	}
	list, err := s.ListSOSRuns(ctx, tenant, 10)
	if err != nil || len(list) != 2 || list[0].SOSID != early {
		t.Fatalf("list, newest first: %+v %v", list, err)
	}
	if list, _ := s.ListSOSRuns(ctx, other, 10); len(list) != 0 {
		t.Errorf("another tenant listed %d runs", len(list))
	}
}

// testAdvanceAlertKeepsAClosedAlertClosed: an escalation step that read an alert before it
// was acknowledged or cancelled must not write it back to escalating (C10: a cancel stops
// escalation for good). Before, the step's compare-and-set checked next_esc_at only.
func testAdvanceAlertKeepsAClosedAlertClosed(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, closed := range []string{store.AlertStateAcknowledged, store.AlertStateCancelled} {
		next := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
		a := &store.Alert{ID: "alert-" + closed, ChainID: "c1", DeviceIMEI: "kit-a", Type: "sos", Detail: "x",
			State: store.AlertStateTriggered, NextEscAt: next, CreatedAt: next, UpdatedAt: next}
		if err := s.CreateAlert(ctx, "t-esc", a); err != nil {
			t.Fatal(err)
		}
		read := *a // what the escalation step read
		a.State, a.AckedBy, a.AckedAt, a.UpdatedAt = closed, "user:u1", next.Add(time.Second), next.Add(time.Second)
		if err := s.UpdateAlert(ctx, "t-esc", a); err != nil {
			t.Fatal(err)
		}
		read.State, read.CurrentTier, read.NextEscAt, read.UpdatedAt = store.AlertStateEscalating, 1, next.Add(time.Minute), next.Add(2*time.Second)
		ok, err := s.AdvanceAlert(ctx, "t-esc", &read, next)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Errorf("%s: the step advanced a closed alert", closed)
		}
		got, err := s.GetAlert(ctx, "t-esc", a.ID)
		if err != nil || got.State != closed {
			t.Errorf("%s: state now %q (%v)", closed, got.State, err)
		}
	}
	// An open alert still advances.
	next := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	a := &store.Alert{ID: "alert-open", ChainID: "c1", Type: "sos", State: store.AlertStateTriggered, NextEscAt: next,
		CreatedAt: next, UpdatedAt: next}
	if err := s.CreateAlert(ctx, "t-esc", a); err != nil {
		t.Fatal(err)
	}
	step := *a
	step.State, step.CurrentTier, step.NextEscAt = store.AlertStateEscalating, 1, next.Add(time.Minute)
	if ok, err := s.AdvanceAlert(ctx, "t-esc", &step, next); err != nil || !ok {
		t.Errorf("an open alert did not advance: %v %v", ok, err)
	}
}
