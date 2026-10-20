package storetest

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/escalation"
	"github.com/meshsat/meshsat-hub/internal/hubmsg"
	"github.com/meshsat/meshsat-hub/internal/sos"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// The r17 review's C10-R6, on every driver (MESHSAT-1584): a cancel closes its run's alert even
// before the alert exists (a tombstone under the run's id), so a start still raising it, a worker
// that raises it after a restart, or the fallback of a start that was not stored never leaves an
// open alert once the cancel is in. The id's uniqueness decides the races, so they are checked
// against each driver's own duplicate-key behaviour.

const r17Chain = "chain-r17"

var r17At = time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)

func r17Alert(id string) *store.Alert {
	return &store.Alert{ID: id, ChainID: r17Chain, DeviceIMEI: "kit-r17", Type: "sos", Detail: "SOS"}
}

func r17State(t *testing.T, s store.Store, tenantID, id string) string {
	t.Helper()
	a, err := s.GetAlert(context.Background(), tenantID, id)
	if err != nil {
		t.Fatalf("alert %s: %v", id, err)
	}
	return a.State
}

// raceStore lets a raise insert the alert between Close's read and its tombstone insert.
type raceStore struct {
	store.Store
	race func()
}

func (s *raceStore) CreateAlert(ctx context.Context, tenantID string, a *store.Alert) error {
	if s.race != nil && a.State == store.AlertStateCancelled {
		f := s.race
		s.race = nil
		f()
	}
	return s.Store.CreateAlert(ctx, tenantID, a)
}

// testEscalationCloseTombstones: Close on an alert that does not exist writes it cancelled, a
// later raise neither replaces it nor pages; a raised alert is cancelled; a cancel that loses
// the insert race to a raise still closes the raised alert.
func testEscalationCloseTombstones(t *testing.T, s store.Store) {
	ctx := context.Background()
	e := escalation.New(s, nil)

	if ok, err := e.Close(ctx, tenant, r17Alert("sos-r17-tomb"), "bridge:kit-r17"); err != nil || !ok {
		t.Fatalf("a cancel before its alert wrote nothing: %v %v", ok, err)
	}
	if created, err := e.TriggerOnce(ctx, tenant, r17Alert("sos-r17-tomb")); err != nil || created {
		t.Fatalf("a raise after the cancel was created: %v %v", created, err)
	}
	if st := r17State(t, s, tenant, "sos-r17-tomb"); st != store.AlertStateCancelled {
		t.Fatalf("a raise after the cancel reopened it: %s", st)
	}
	if ok, err := e.Close(ctx, tenant, r17Alert("sos-r17-tomb"), "bridge:kit-r17"); err != nil || ok {
		t.Fatalf("closing a closed alert again: %v %v", ok, err)
	}

	if created, err := e.TriggerOnce(ctx, tenant, r17Alert("sos-r17-raised")); err != nil || !created {
		t.Fatalf("the raise: %v %v", created, err)
	}
	if ok, err := e.Close(ctx, tenant, r17Alert("sos-r17-raised"), "bridge:kit-r17"); err != nil || !ok {
		t.Fatalf("closing a raised alert: %v %v", ok, err)
	}
	if st := r17State(t, s, tenant, "sos-r17-raised"); st != store.AlertStateCancelled {
		t.Fatalf("the raised alert after the cancel: %s", st)
	}

	raced := false
	rs := &raceStore{Store: s, race: func() {
		raced = true
		if created, err := escalation.New(s, nil).TriggerOnce(ctx, tenant, r17Alert("sos-r17-race")); err != nil || !created {
			t.Errorf("the racing raise: %v %v", created, err)
		}
	}}
	if ok, err := escalation.New(rs, nil).Close(ctx, tenant, r17Alert("sos-r17-race"), "bridge:kit-r17"); err != nil || !ok {
		t.Fatalf("a cancel that lost the insert race: %v %v", ok, err)
	}
	if !raced {
		t.Fatal("the precondition: no raise raced the tombstone")
	}
	if st := r17State(t, s, tenant, "sos-r17-race"); st != store.AlertStateCancelled {
		t.Fatalf("a cancel that lost the insert race left the alert %s", st)
	}
}

// nopBus takes the run service's publishes (acks, consumer events) and drops them.
type nopBus struct{}

func (nopBus) Connect() error                                                { return nil }
func (nopBus) Publish(string, byte, bool, []byte) error                      { return nil }
func (nopBus) PublishJSON(string, byte, bool, any) error                     { return nil }
func (nopBus) Subscribe(string, byte, bus.MessageHandler) error              { return nil }
func (nopBus) QueueSubscribe(string, byte, string, bus.MessageHandler) error { return nil }
func (nopBus) IsConnected() bool                                             { return true }
func (nopBus) Disconnect()                                                   {}

// overlapStore runs a cancel between a start's alert read and its insert, and fails the start's
// next run read once (the review's reproduction A).
type overlapStore struct {
	store.Store
	beforeCreate func()
	failRead     bool
}

func (s *overlapStore) CreateAlert(ctx context.Context, tenantID string, a *store.Alert) error {
	if s.beforeCreate != nil {
		f := s.beforeCreate
		s.beforeCreate = nil
		f()
	}
	err := s.Store.CreateAlert(ctx, tenantID, a)
	s.failRead = true
	return err
}

func (s *overlapStore) GetSOSRun(ctx context.Context, tenantID, origin, sosID string) (*store.SOSRun, error) {
	if s.failRead {
		s.failRead = false
		return nil, errors.New("storetest: transient read failure after the raise")
	}
	return s.Store.GetSOSRun(ctx, tenantID, origin, sosID)
}

func r17Runs(s store.Store) *sos.Runs {
	return sos.NewRuns(s, nopBus{}, escalation.New(s, nil), func(string) string { return r17Chain }, nil)
}

func r17Start(tenantID, bridgeID string) sos.Event {
	const ms = 1792330000123
	return sos.Event{TenantID: tenantID, BridgeID: bridgeID, SOSID: hubmsg.UUIDv5(bridgeID + ":" + strconv.FormatInt(ms, 10)),
		RunStartMs: ms, Bearer: "mqtt", Kind: store.SOSEventStart, Text: "SOS"}
}

func r17Cancel(ev sos.Event) sos.Event {
	ev.Kind = store.SOSEventCancel
	return ev
}

// r17Settled checks the end state every case must reach: the run cancelled, no alert work
// owed, nothing left for the reconciler, and the run's alert cancelled.
func r17Settled(t *testing.T, s store.Store, runs *sos.Runs, ev sos.Event) {
	t.Helper()
	ctx := context.Background()
	if _, err := runs.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := s.GetSOSRun(ctx, ev.TenantID, sos.Origin(ev.BridgeID), ev.SOSID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != store.SOSRunCancelled || run.AlertPending {
		t.Fatalf("the run: state %s, alert work owed %v", run.State, run.AlertPending)
	}
	id := sos.AlertID(ev.TenantID, sos.Origin(ev.BridgeID), ev.SOSID)
	if st := r17State(t, s, ev.TenantID, id); st != store.AlertStateCancelled {
		t.Fatalf("the cancel was acked and settled, and its run's alert is %s", st)
	}
	// Nor under any other id: every case of the tenant ends with nothing that can page.
	open, err := s.ListAlerts(ctx, ev.TenantID, true, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range open {
		t.Errorf("alert %s is %s after the cancel", a.ID, a.State)
	}
}

// testSOSCancelFencesLateAlertWork: the review's two reproductions and the crash variants, on
// the real run service over this driver.
func testSOSCancelFencesLateAlertWork(t *testing.T, s store.Store) {
	ctx := context.Background()
	const tn = "t-r17"
	if err := s.CreateEscalationChain(ctx, tn, &store.EscalationChain{ID: r17Chain, Name: "on call",
		Tiers: []store.EscalationTier{{Name: "sms", Targets: []string{"x"}, WaitSec: 60}}}); err != nil {
		t.Fatal(err)
	}

	t.Run("a cancel that settles while the start raises", func(t *testing.T) {
		ev := r17Start(tn, "kit-r17-a")
		ovl := &overlapStore{Store: s}
		runs := r17Runs(ovl)
		ovl.beforeCreate = func() {
			if _, err := runs.Apply(ctx, r17Cancel(ev)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := runs.Apply(ctx, ev); err != nil {
			t.Fatal(err)
		}
		r17Settled(t, s, r17Runs(s), ev)
	})

	t.Run("the fallback of a start after a cancel-first run", func(t *testing.T) {
		ev := r17Start(tn, "kit-r17-b")
		runs := r17Runs(s)
		if _, err := runs.Apply(ctx, r17Cancel(ev)); err != nil {
			t.Fatal(err)
		}
		if !runs.PageUnstored(ctx, ev) {
			t.Fatal("the fallback refused")
		}
		r17Settled(t, s, runs, ev)
	})

	t.Run("a crash after a stale insertion, then the cancel", func(t *testing.T) {
		// The start is stored and its alert inserted; the process dies before anything else.
		ev := r17Start(tn, "kit-r17-c")
		if _, err := s.ApplySOSRunEvent(ctx, &store.SOSRunEvent{TenantID: tn, Origin: sos.Origin(ev.BridgeID), SOSID: ev.SOSID,
			BridgeID: ev.BridgeID, RunStartMs: ev.RunStartMs, Kind: store.SOSEventStart, Bearer: "mqtt", At: r17At}); err != nil {
			t.Fatal(err)
		}
		if created, err := escalation.New(s, nil).TriggerOnce(ctx, tn, r17Alert(sos.AlertID(tn, sos.Origin(ev.BridgeID), ev.SOSID))); err != nil || !created {
			t.Fatalf("the stale insertion: %v %v", created, err)
		}
		// The restarted process takes the cancel.
		runs := r17Runs(s)
		if _, err := runs.Apply(ctx, r17Cancel(ev)); err != nil {
			t.Fatal(err)
		}
		r17Settled(t, s, runs, ev)
	})

	t.Run("a stale raise after the cancel settled and a restart", func(t *testing.T) {
		ev := r17Start(tn, "kit-r17-d")
		runs := r17Runs(s)
		if _, err := runs.Apply(ctx, r17Cancel(ev)); err != nil {
			t.Fatal(err)
		}
		// A worker that read the run active before the cancel raises its alert after a restart.
		if created, err := escalation.New(s, nil).TriggerOnce(ctx, tn, r17Alert(sos.AlertID(tn, sos.Origin(ev.BridgeID), ev.SOSID))); err != nil || created {
			t.Fatalf("a stale raise after the cancel was created: %v %v", created, err)
		}
		if _, err := r17Runs(s).Apply(ctx, ev); err != nil {
			t.Fatal(err)
		}
		r17Settled(t, s, r17Runs(s), ev)
	})
}
