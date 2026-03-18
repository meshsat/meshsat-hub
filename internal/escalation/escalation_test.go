package escalation

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/cubeos-app/meshsat-hub/internal/store/sqlite"
)

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestTriggerCreatesAlert(t *testing.T) {
	s := newTestStore(t)
	e := New(s, LogNotifier{})

	// Create a chain first.
	chain := &store.EscalationChain{
		Name: "SOS Chain",
		Tiers: []store.EscalationTier{
			{Name: "tier1", Targets: []string{"oncall@example.com"}, WaitSec: 60, MaxRetries: 3},
			{Name: "tier2", Targets: []string{"manager@example.com"}, WaitSec: 120, MaxRetries: 2},
		},
	}
	if err := s.CreateEscalationChain(context.Background(), "default", chain); err != nil {
		t.Fatal(err)
	}

	alert := &store.Alert{
		ChainID:    chain.ID,
		DeviceIMEI: "300234065123456",
		Type:       "sos",
		Detail:     "SOS button pressed",
	}
	if err := e.Trigger(context.Background(), "default", alert); err != nil {
		t.Fatal(err)
	}

	if alert.ID == "" {
		t.Error("expected non-empty alert ID")
	}
	if alert.State != store.AlertStateTriggered {
		t.Errorf("expected state triggered, got %s", alert.State)
	}
	if alert.CurrentTier != 0 {
		t.Errorf("expected tier 0, got %d", alert.CurrentTier)
	}

	// Verify it's persisted.
	fetched, err := s.GetAlert(context.Background(), "", alert.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fetched.Type != "sos" {
		t.Errorf("expected type sos, got %s", fetched.Type)
	}
}

func TestAcknowledgeStopsEscalation(t *testing.T) {
	s := newTestStore(t)
	e := New(s, LogNotifier{})

	chain := &store.EscalationChain{
		Name:  "Test",
		Tiers: []store.EscalationTier{{Name: "t1", Targets: []string{"x"}, WaitSec: 10, MaxRetries: 3}},
	}
	_ = s.CreateEscalationChain(context.Background(), "default", chain)

	alert := &store.Alert{ChainID: chain.ID, DeviceIMEI: "dev1", Type: "sos", Detail: "test"}
	_ = e.Trigger(context.Background(), "default", alert)

	if err := e.Acknowledge(context.Background(), "default", alert.ID, "operator-1"); err != nil {
		t.Fatal(err)
	}

	fetched, _ := s.GetAlert(context.Background(), "", alert.ID)
	if fetched.State != store.AlertStateAcknowledged {
		t.Errorf("expected acknowledged, got %s", fetched.State)
	}
	if fetched.AckedBy != "operator-1" {
		t.Errorf("expected acked_by operator-1, got %s", fetched.AckedBy)
	}
}

func TestAcknowledgeIdempotent(t *testing.T) {
	s := newTestStore(t)
	e := New(s, LogNotifier{})

	chain := &store.EscalationChain{
		Name:  "Test",
		Tiers: []store.EscalationTier{{Name: "t1", Targets: []string{"x"}, WaitSec: 10, MaxRetries: 1}},
	}
	_ = s.CreateEscalationChain(context.Background(), "default", chain)

	alert := &store.Alert{ChainID: chain.ID, DeviceIMEI: "dev1", Type: "sos"}
	_ = e.Trigger(context.Background(), "default", alert)
	_ = e.Acknowledge(context.Background(), "default", alert.ID, "op1")

	// Second ack should not error.
	if err := e.Acknowledge(context.Background(), "default", alert.ID, "op2"); err != nil {
		t.Errorf("second ack should not error: %v", err)
	}
}

// recordingNotifier captures notification calls.
type recordingNotifier struct {
	mu    sync.Mutex
	calls []notifyCall
}

type notifyCall struct {
	Targets []string
	Subject string
	Body    string
}

func (r *recordingNotifier) Notify(_ context.Context, targets []string, subject, body string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, notifyCall{Targets: targets, Subject: subject, Body: body})
	return nil
}

func (r *recordingNotifier) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func TestProcessAlertEscalatesThroughTiers(t *testing.T) {
	s := newTestStore(t)
	notifier := &recordingNotifier{}
	e := New(s, notifier)

	chain := &store.EscalationChain{
		Name: "Two Tiers",
		Tiers: []store.EscalationTier{
			{Name: "tier1", Targets: []string{"a@test.com"}, WaitSec: 0, MaxRetries: 1},
			{Name: "tier2", Targets: []string{"b@test.com"}, WaitSec: 0, MaxRetries: 1},
		},
	}
	_ = s.CreateEscalationChain(context.Background(), "default", chain)

	alert := &store.Alert{ChainID: chain.ID, DeviceIMEI: "dev1", Type: "sos", Detail: "help"}
	_ = e.Trigger(context.Background(), "default", alert)

	now := time.Now().UTC()

	// Process: should notify tier1 and move to tier2.
	e.processAlert(context.Background(), alert, now)

	// Refresh from DB.
	alert, _ = s.GetAlert(context.Background(), "", alert.ID)
	if alert.CurrentTier != 1 {
		t.Errorf("expected tier 1 after first process, got %d", alert.CurrentTier)
	}

	// Process again: should notify tier2 and exhaust.
	e.processAlert(context.Background(), alert, now.Add(time.Second))
	alert, _ = s.GetAlert(context.Background(), "", alert.ID)
	if alert.State != store.AlertStateExhausted {
		t.Errorf("expected exhausted, got %s", alert.State)
	}

	if notifier.callCount() != 2 {
		t.Errorf("expected 2 notify calls, got %d", notifier.callCount())
	}
}

func TestProcessAlertRetriesWithinTier(t *testing.T) {
	s := newTestStore(t)
	notifier := &recordingNotifier{}
	e := New(s, notifier)

	chain := &store.EscalationChain{
		Name: "Retry Chain",
		Tiers: []store.EscalationTier{
			{Name: "tier1", Targets: []string{"a@test.com"}, WaitSec: 5, MaxRetries: 3},
		},
	}
	_ = s.CreateEscalationChain(context.Background(), "default", chain)

	alert := &store.Alert{ChainID: chain.ID, DeviceIMEI: "dev1", Type: "sos"}
	_ = e.Trigger(context.Background(), "default", alert)

	now := time.Now().UTC()

	// Process 3 times (maxRetries=3). Each should stay in tier 0 until exhausted.
	for i := 0; i < 3; i++ {
		alert, _ = s.GetAlert(context.Background(), "", alert.ID)
		e.processAlert(context.Background(), alert, now.Add(time.Duration(i)*time.Minute))
	}

	alert, _ = s.GetAlert(context.Background(), "", alert.ID)
	if alert.State != store.AlertStateExhausted {
		t.Errorf("expected exhausted after 3 retries, got %s (tier=%d, retries=%d)",
			alert.State, alert.CurrentTier, alert.Retries)
	}
	if notifier.callCount() != 3 {
		t.Errorf("expected 3 notify calls, got %d", notifier.callCount())
	}
}

func TestListActiveAlerts(t *testing.T) {
	s := newTestStore(t)
	e := New(s, LogNotifier{})

	chain := &store.EscalationChain{
		Name:  "Test",
		Tiers: []store.EscalationTier{{Name: "t1", Targets: []string{"x"}, WaitSec: 60, MaxRetries: 3}},
	}
	_ = s.CreateEscalationChain(context.Background(), "default", chain)

	// Create two alerts, ack one.
	a1 := &store.Alert{ChainID: chain.ID, DeviceIMEI: "dev1", Type: "sos"}
	a2 := &store.Alert{ChainID: chain.ID, DeviceIMEI: "dev2", Type: "sos"}
	_ = e.Trigger(context.Background(), "default", a1)
	_ = e.Trigger(context.Background(), "default", a2)
	_ = e.Acknowledge(context.Background(), "default", a1.ID, "op")

	active, err := s.ListAlerts(context.Background(), "default", true, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 {
		t.Errorf("expected 1 active alert, got %d", len(active))
	}

	all, err := s.ListAlerts(context.Background(), "default", false, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("expected 2 total alerts, got %d", len(all))
	}
}

func TestChainCRUD(t *testing.T) {
	s := newTestStore(t)

	chain := &store.EscalationChain{
		Name: "Test Chain",
		Tiers: []store.EscalationTier{
			{Name: "sms", Targets: []string{"+31612345678"}, WaitSec: 60, MaxRetries: 2},
			{Name: "email", Targets: []string{"ops@example.com"}, WaitSec: 300, MaxRetries: 1},
		},
	}
	ctx := context.Background()

	if err := s.CreateEscalationChain(ctx, "default", chain); err != nil {
		t.Fatal(err)
	}

	// List
	chains, err := s.ListEscalationChains(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(chains) != 1 {
		t.Fatalf("expected 1 chain, got %d", len(chains))
	}
	if chains[0].Name != "Test Chain" {
		t.Errorf("expected name Test Chain, got %s", chains[0].Name)
	}
	if len(chains[0].Tiers) != 2 {
		t.Errorf("expected 2 tiers, got %d", len(chains[0].Tiers))
	}

	// Get
	fetched, err := s.GetEscalationChain(ctx, "default", chain.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fetched.Tiers[0].Name != "sms" {
		t.Errorf("expected tier name sms, got %s", fetched.Tiers[0].Name)
	}

	// Delete
	if err := s.DeleteEscalationChain(ctx, "default", chain.ID); err != nil {
		t.Fatal(err)
	}
	chains, _ = s.ListEscalationChains(ctx, "default")
	if len(chains) != 0 {
		t.Errorf("expected 0 chains after delete, got %d", len(chains))
	}
}
