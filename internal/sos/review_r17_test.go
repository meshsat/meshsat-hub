package sos

// The r17 review's reproductions of C10-R6, as the reviewer wrote them (MESHSAT-1584): a cancel
// that settles while an older start is raising its alert, and the start fallback after a
// cancel-first run.

import (
	"context"
	"errors"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/escalation"
	"github.com/meshsat/meshsat-hub/internal/store"
)

type reviewOverlapStore struct {
	store.Store
	beforeCreate func()
	failRead     bool
}

func (s *reviewOverlapStore) CreateAlert(c context.Context, tenant string, a *store.Alert) error {
	if s.beforeCreate != nil {
		f := s.beforeCreate
		s.beforeCreate = nil
		f()
	}
	err := s.Store.CreateAlert(c, tenant, a)
	s.failRead = true
	return err
}

func (s *reviewOverlapStore) GetSOSRun(c context.Context, tenant, origin, id string) (*store.SOSRun, error) {
	if s.failRead {
		s.failRead = false
		return nil, errors.New("review: transient post-raise read failure")
	}
	return s.Store.GetSOSRun(c, tenant, origin, id)
}

func TestReviewR17OverlappingCancelCannotLoseAlertWork(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ev := legacyStart(loadFixture(t))
	st := &reviewOverlapStore{Store: h.st}
	h.runs.store = st
	h.runs.engine = escalation.New(st, nil)
	st.beforeCreate = func() {
		cancel := ev
		cancel.Kind = store.SOSEventCancel
		if _, err := h.runs.Apply(ctx, cancel); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.runs.Apply(ctx, ev); err != nil {
		t.Fatal(err)
	}
	n, err := h.runs.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := h.st.GetSOSRun(ctx, tenant, Origin(ev.BridgeID), ev.SOSID)
	if err != nil {
		t.Fatal(err)
	}
	alerts := h.alerts(t)
	t.Logf("run=%s pending=%v reconciled=%d alert=%s", run.State, run.AlertPending, n, alerts[0].State)
	if run.State == store.SOSRunCancelled && alerts[0].State != store.AlertStateCancelled {
		t.Fatal("cancel acknowledged and pending cleared, but late alert remains open after recovery")
	}
}

func TestReviewR17FallbackCannotPageAfterCancelledRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ev := legacyStart(loadFixture(t))
	cancel := ev
	cancel.Kind = store.SOSEventCancel
	if _, err := h.runs.Apply(ctx, cancel); err != nil {
		t.Fatal(err)
	}
	if !h.runs.PageUnstored(ctx, ev) {
		t.Fatal("fallback unexpectedly refused")
	}
	n, err := h.runs.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a := h.alerts(t)
	if len(a) > 0 && a[0].State != store.AlertStateCancelled {
		t.Fatalf("cancel-before-start bypassed by fallback: alert=%s reconciled=%d", a[0].State, n)
	}
}
