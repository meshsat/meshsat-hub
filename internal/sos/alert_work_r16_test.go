package sos

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/meshsat/meshsat-hub/internal/escalation"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// The r16 review's C10-R2 (MESHSAT-1584): the alert work a run's state change owes is
// committed with the run, and an ack the sender gets stands for it. What an apply could not
// finish, the reconciler does, without another copy from the sender.

// failAlerts fails the alert writes it is told to, as a database that breaks between the
// run's commit and its alert.
type failAlerts struct {
	store.Store
	mu                    sync.Mutex
	failCreate, failClose bool
	failApply             bool
	transientApply        int // this many run writes fail as a dropped connection first
}

func (s *failAlerts) set(create, closeAlert, apply bool) {
	s.mu.Lock()
	s.failCreate, s.failClose, s.failApply = create, closeAlert, apply
	s.mu.Unlock()
}

func (s *failAlerts) CreateAlert(ctx context.Context, tenantID string, a *store.Alert) error {
	s.mu.Lock()
	fail := s.failCreate
	s.mu.Unlock()
	if fail {
		return errors.New("injected: alert insert failed")
	}
	return s.Store.CreateAlert(ctx, tenantID, a)
}

func (s *failAlerts) UpdateAlert(ctx context.Context, tenantID string, a *store.Alert) error {
	s.mu.Lock()
	fail := s.failClose && a.State == store.AlertStateCancelled
	s.mu.Unlock()
	if fail {
		return errors.New("injected: alert close failed")
	}
	return s.Store.UpdateAlert(ctx, tenantID, a)
}

func (s *failAlerts) ApplySOSRunEvent(ctx context.Context, ev *store.SOSRunEvent) (store.SOSRunApplied, error) {
	s.mu.Lock()
	fail := s.failApply
	transient := s.transientApply > 0
	if transient {
		s.transientApply--
	}
	s.mu.Unlock()
	if transient {
		return store.SOSRunApplied{}, fmt.Errorf("injected: %w", driver.ErrBadConn)
	}
	if fail {
		return store.SOSRunApplied{}, errors.New("injected: run not stored")
	}
	return s.Store.ApplySOSRunEvent(ctx, ev)
}

func failing(t *testing.T) (*harness, *failAlerts) {
	t.Helper()
	h := newHarness(t)
	fs := &failAlerts{Store: h.st}
	h.runs.store = fs
	h.runs.engine = escalation.New(fs, nil)
	return h, fs
}

func legacyStart(f fixture) Event {
	return Event{TenantID: tenant, BridgeID: f.Legacy.BridgeID, SOSID: f.Legacy.SOSID, RunStartMs: f.Legacy.RunStartMs,
		Bearer: "mqtt", Kind: store.SOSEventStart, Text: "Injured, need help"}
}

func (h *harness) run(t *testing.T, f fixture) *store.SOSRun {
	t.Helper()
	r, err := h.st.GetSOSRun(context.Background(), tenant, Origin(f.Legacy.BridgeID), f.Legacy.SOSID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (h *harness) reconcile(t *testing.T) int {
	t.Helper()
	n, err := h.runs.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A start whose alert insert fails is still acknowledged: the run and its owed alert work are
// committed. Once the database recovers, the reconciler raises exactly one alert.
func TestR16AFailedRaiseIsReconciledAfterItsAck(t *testing.T) {
	f := loadFixture(t)
	h, fs := failing(t)
	ctx := context.Background()
	fs.set(true, false, false)
	if _, err := h.runs.Apply(ctx, legacyStart(f)); err != nil {
		t.Fatalf("the run itself was stored, yet: %v", err)
	}
	if n := len(h.bus.on("/sos/ack")); n != 1 {
		t.Fatalf("%d acks", n)
	}
	if n := len(h.alerts(t)); n != 0 || !h.run(t, f).AlertPending {
		t.Fatalf("%d alerts, pending %v", n, h.run(t, f).AlertPending)
	}
	if n := h.reconcile(t); n != 0 || len(h.alerts(t)) != 0 {
		t.Fatalf("settled %d while the database still fails", n)
	}
	fs.set(false, false, false)
	if n := h.reconcile(t); n != 1 {
		t.Fatalf("settled %d after recovery, want 1", n)
	}
	alerts := h.alerts(t)
	run := h.run(t, f)
	if len(alerts) != 1 || alerts[0].ID != AlertID(tenant, run.Origin, run.SOSID) || run.AlertPending || run.AlertID != alerts[0].ID {
		t.Fatalf("after the reconciler: alerts %+v run %+v", alerts, run)
	}
	if n := h.reconcile(t); n != 0 || len(h.alerts(t)) != 1 {
		t.Fatalf("a second pass changed things: settled %d, %d alerts", n, len(h.alerts(t)))
	}
}

// A cancel whose alert close fails is acknowledged and the alert is closed by the reconciler.
func TestR16AFailedCloseIsReconciledAfterItsAck(t *testing.T) {
	f := loadFixture(t)
	h, fs := failing(t)
	ctx := context.Background()
	if _, err := h.runs.Apply(ctx, legacyStart(f)); err != nil {
		t.Fatal(err)
	}
	fs.set(false, true, false)
	cancel := legacyStart(f)
	cancel.Kind = store.SOSEventCancel
	if _, err := h.runs.Apply(ctx, cancel); err != nil {
		t.Fatal(err)
	}
	if n := len(h.bus.on("/sos/ack")); n != 2 {
		t.Fatalf("%d acks, want 2", n)
	}
	if a := h.alerts(t); len(a) != 1 || a[0].State == store.AlertStateCancelled {
		t.Fatalf("the close was injected to fail, alerts %+v", a)
	}
	fs.set(false, false, false)
	if n := h.reconcile(t); n != 1 {
		t.Fatalf("settled %d", n)
	}
	if a := h.alerts(t); len(a) != 1 || a[0].State != store.AlertStateCancelled || h.run(t, f).AlertPending {
		t.Fatalf("after the reconciler: %+v", a)
	}
}

// A replica that stopped after committing the run and before its alert work: a fresh service
// on the same database settles it, for the start and for the cancel.
func TestR16ACrashBetweenTheRunAndItsAlertIsReconciled(t *testing.T) {
	f := loadFixture(t)
	h := newHarness(t)
	ctx := context.Background()
	ev := func(kind string) *store.SOSRunEvent {
		return &store.SOSRunEvent{TenantID: tenant, Origin: Origin(f.Legacy.BridgeID), SOSID: f.Legacy.SOSID,
			BridgeID: f.Legacy.BridgeID, RunStartMs: f.Legacy.RunStartMs, Kind: kind, Bearer: "sbd", At: time.Now().UTC()}
	}
	if _, err := h.st.ApplySOSRunEvent(ctx, ev(store.SOSEventStart)); err != nil {
		t.Fatal(err)
	}
	if n := h.reconcile(t); n != 1 || len(h.alerts(t)) != 1 {
		t.Fatalf("start: settled %d, %d alerts", n, len(h.alerts(t)))
	}
	if _, err := h.st.ApplySOSRunEvent(ctx, ev(store.SOSEventCancel)); err != nil {
		t.Fatal(err)
	}
	if n := h.reconcile(t); n != 1 {
		t.Fatalf("cancel: settled %d", n)
	}
	if a := h.alerts(t); len(a) != 1 || a[0].State != store.AlertStateCancelled {
		t.Fatalf("cancel: alerts %+v", a)
	}
}

// An event applied while the alert work of an older rev ran keeps the work owed: the clear is
// for the rev the work was done for.
func TestR16AnEventDuringTheAlertWorkKeepsItOwed(t *testing.T) {
	f := loadFixture(t)
	h := newHarness(t)
	ctx := context.Background()
	applied, err := h.st.ApplySOSRunEvent(ctx, &store.SOSRunEvent{TenantID: tenant, Origin: Origin(f.Legacy.BridgeID),
		SOSID: f.Legacy.SOSID, BridgeID: f.Legacy.BridgeID, RunStartMs: f.Legacy.RunStartMs, Kind: store.SOSEventStart,
		Bearer: "mqtt", At: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	stale := applied.Run // rev 1
	if _, err := h.st.ApplySOSRunEvent(ctx, &store.SOSRunEvent{TenantID: tenant, Origin: Origin(f.Legacy.BridgeID),
		SOSID: f.Legacy.SOSID, BridgeID: f.Legacy.BridgeID, RunStartMs: f.Legacy.RunStartMs, Kind: store.SOSEventCancel,
		Bearer: "mqtt", By: Origin(f.Legacy.BridgeID), At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if h.runs.settle(ctx, stale) {
		t.Fatal("the stale rev's work cleared the obligation the cancel made")
	}
	if !h.run(t, f).AlertPending {
		t.Fatal("not owed after the stale clear")
	}
	if n := h.reconcile(t); n != 1 {
		t.Fatalf("settled %d", n)
	}
	if a := h.alerts(t); len(a) != 1 || a[0].State != store.AlertStateCancelled {
		t.Fatalf("alerts %+v", a)
	}
}

// A valid start the database does not take over MQTT is neither acknowledged nor paged the
// old way (a second identity): the bridge publishes it again until it has its ack.
func TestR16AStartNotStoredIsRepeatedNotPagedTheOldWay(t *testing.T) {
	f := loadFixture(t)
	h, fs := failing(t)
	fs.set(false, false, true)
	payload, _ := json.Marshal(map[string]any{"v": 2, "kind": "start", "sos_id": f.Legacy.SOSID,
		"run_start_ms": f.Legacy.RunStartMs, "text": "Injured, need help"})
	h.runs.handleMQTT("meshsat/bridge/"+f.Legacy.BridgeID+"/sos/event", payload)
	if n, m := len(h.bus.on("/sos/ack")), len(h.bus.on("/mo/decoded")); n != 0 || m != 0 {
		t.Fatalf("%d acks, %d old-way pages for a start the database did not take", n, m)
	}
	fs.set(false, false, false)
	h.runs.handleMQTT("meshsat/bridge/"+f.Legacy.BridgeID+"/sos/event", payload)
	if n, a := len(h.bus.on("/sos/ack")), len(h.alerts(t)); n != 1 || a != 1 {
		t.Fatalf("the repeat: %d acks, %d alerts", n, a)
	}
}

// A transient database error on the run's write (a dropped connection, a serialization
// failure) is retried at once: the start is stored, acked and paged once, without waiting
// for the sender's next copy.
func TestR16ATransientErrorOnTheRunsWriteIsRetried(t *testing.T) {
	f := loadFixture(t)
	h, fs := failing(t)
	fs.mu.Lock()
	fs.transientApply = 1
	fs.mu.Unlock()
	payload, _ := json.Marshal(map[string]any{"v": 2, "kind": "start", "sos_id": f.Legacy.SOSID,
		"run_start_ms": f.Legacy.RunStartMs, "text": "Injured, need help"})
	h.runs.handleMQTT("meshsat/bridge/"+f.Legacy.BridgeID+"/sos/event", payload)
	acks, alerts, legacy := len(h.bus.on("/sos/ack")), len(h.alerts(t)), len(h.bus.on("/mo/decoded"))
	if acks != 1 || alerts != 1 || legacy != 0 {
		t.Fatalf("after one dropped connection: %d acks, %d alerts, %d old-way pages; want 1, 1, 0", acks, alerts, legacy)
	}
	fs.mu.Lock()
	left := fs.transientApply
	fs.mu.Unlock()
	if left != 0 {
		t.Fatal("the injected failure was never hit")
	}
}

// A valid start that could not be stored pages under its run's own id; the run, once a later
// copy stores it, finds the alert raised and pages nobody again.
func TestR16PageUnstoredPagesUnderTheRunsIDOnce(t *testing.T) {
	f := loadFixture(t)
	h := newHarness(t)
	ev := legacyStart(f)
	ev.Bearer = "sbd"
	if !h.runs.PageUnstored(context.Background(), ev) {
		t.Fatal("not paged")
	}
	if _, err := h.runs.Apply(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	alerts := h.alerts(t)
	run := h.run(t, f)
	if len(alerts) != 1 || alerts[0].ID != AlertID(tenant, run.Origin, run.SOSID) || run.AlertID != alerts[0].ID || run.AlertPending {
		t.Fatalf("alerts %+v run %+v", alerts, run)
	}
}

// The reconciler runs at once when it starts (a restart, a new leader).
func TestR16TheReconcilerRunsAtStart(t *testing.T) {
	f := loadFixture(t)
	h := newHarness(t)
	if _, err := h.st.ApplySOSRunEvent(context.Background(), &store.SOSRunEvent{TenantID: tenant,
		Origin: Origin(f.Legacy.BridgeID), SOSID: f.Legacy.SOSID, BridgeID: f.Legacy.BridgeID,
		RunStartMs: f.Legacy.RunStartMs, Kind: store.SOSEventStart, Bearer: "imt", At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.runs.RunReconciler(ctx, time.Hour); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(h.alerts(t)) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if len(h.alerts(t)) != 1 {
		t.Fatal("the reconciler did not run at start")
	}
}
