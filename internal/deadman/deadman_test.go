package deadman

import (
	"context"
	"testing"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/escalation"
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

func TestMissedCheckin_TriggersAlert(t *testing.T) {
	s := newTestStore(t)
	engine := escalation.New(s, escalation.LogNotifier{})
	m := NewMonitor(s, engine)
	ctx := context.Background()

	// Create chain + device with old last_seen.
	chain := &store.EscalationChain{
		Name:  "Test",
		Tiers: []store.EscalationTier{{Name: "t1", Targets: []string{"test"}, WaitSec: 60, MaxRetries: 1}},
	}
	_ = s.CreateEscalationChain(ctx, "default", chain)

	_ = s.CreateDevice(ctx, "default", &store.Device{IMEI: "dev1"})
	// Set last_seen to 2 hours ago.
	oldTime := time.Now().Add(-2 * time.Hour)
	dev, _ := s.GetDevice(ctx, "default", "dev1")
	dev.LastSeen = oldTime
	_ = s.UpdateDevice(ctx, "default", dev)

	m.Configure(Config{
		DeviceIMEI: "dev1",
		ChainID:    chain.ID,
		Interval:   1 * time.Hour,
		Grace:      10 * time.Minute,
		Enabled:    true,
	})

	// Scan should trigger alert.
	m.scan(ctx)

	// Verify alert was created.
	alerts, err := s.ListAlerts(ctx, "default", true, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("expected 1 alert, got %d", len(alerts))
	}
	if alerts[0].Type != "deadman" {
		t.Errorf("expected type deadman, got %s", alerts[0].Type)
	}
}

func TestRecentCheckin_NoAlert(t *testing.T) {
	s := newTestStore(t)
	engine := escalation.New(s, escalation.LogNotifier{})
	m := NewMonitor(s, engine)
	ctx := context.Background()

	chain := &store.EscalationChain{
		Name:  "Test",
		Tiers: []store.EscalationTier{{Name: "t1", Targets: []string{"test"}, WaitSec: 60, MaxRetries: 1}},
	}
	_ = s.CreateEscalationChain(ctx, "default", chain)

	_ = s.CreateDevice(ctx, "default", &store.Device{IMEI: "dev2"})
	// TouchDeviceLastSeen sets it to now, which is within the 1h window.
	_ = s.TouchDeviceLastSeen(ctx, "default", "dev2")

	m.Configure(Config{
		DeviceIMEI: "dev2",
		ChainID:    chain.ID,
		Interval:   1 * time.Hour,
		Grace:      10 * time.Minute,
		Enabled:    true,
	})

	m.scan(ctx)

	alerts, _ := s.ListAlerts(ctx, "default", true, 10)
	if len(alerts) != 0 {
		t.Errorf("expected 0 alerts for recent checkin, got %d", len(alerts))
	}
}

func TestSnooze_SuppressesAlert(t *testing.T) {
	s := newTestStore(t)
	engine := escalation.New(s, escalation.LogNotifier{})
	m := NewMonitor(s, engine)
	ctx := context.Background()

	chain := &store.EscalationChain{
		Name:  "Test",
		Tiers: []store.EscalationTier{{Name: "t1", Targets: []string{"test"}, WaitSec: 60, MaxRetries: 1}},
	}
	_ = s.CreateEscalationChain(ctx, "default", chain)

	_ = s.CreateDevice(ctx, "default", &store.Device{IMEI: "dev3"})
	dev, _ := s.GetDevice(ctx, "default", "dev3")
	dev.LastSeen = time.Now().Add(-2 * time.Hour)
	_ = s.UpdateDevice(ctx, "default", dev)

	m.Configure(Config{
		DeviceIMEI: "dev3",
		ChainID:    chain.ID,
		Interval:   1 * time.Hour,
		Grace:      10 * time.Minute,
		Enabled:    true,
	})

	// Snooze for 1 hour.
	m.Snooze("dev3", 1*time.Hour)

	m.scan(ctx)

	alerts, _ := s.ListAlerts(ctx, "default", true, 10)
	if len(alerts) != 0 {
		t.Errorf("expected 0 alerts while snoozed, got %d", len(alerts))
	}
}

func TestDuplicateAlert_NotTriggered(t *testing.T) {
	s := newTestStore(t)
	engine := escalation.New(s, escalation.LogNotifier{})
	m := NewMonitor(s, engine)
	ctx := context.Background()

	chain := &store.EscalationChain{
		Name:  "Test",
		Tiers: []store.EscalationTier{{Name: "t1", Targets: []string{"test"}, WaitSec: 60, MaxRetries: 1}},
	}
	_ = s.CreateEscalationChain(ctx, "default", chain)

	_ = s.CreateDevice(ctx, "default", &store.Device{IMEI: "dev4"})
	dev, _ := s.GetDevice(ctx, "default", "dev4")
	dev.LastSeen = time.Now().Add(-2 * time.Hour)
	_ = s.UpdateDevice(ctx, "default", dev)

	m.Configure(Config{
		DeviceIMEI: "dev4",
		ChainID:    chain.ID,
		Interval:   1 * time.Hour,
		Grace:      10 * time.Minute,
		Enabled:    true,
	})

	// Scan twice — should only create one alert.
	m.scan(ctx)
	m.scan(ctx)

	alerts, _ := s.ListAlerts(ctx, "default", false, 10)
	if len(alerts) != 1 {
		t.Errorf("expected 1 alert (no duplicate), got %d", len(alerts))
	}
}

func TestClearAlert_AllowsRetrigger(t *testing.T) {
	s := newTestStore(t)
	engine := escalation.New(s, escalation.LogNotifier{})
	m := NewMonitor(s, engine)
	ctx := context.Background()

	chain := &store.EscalationChain{
		Name:  "Test",
		Tiers: []store.EscalationTier{{Name: "t1", Targets: []string{"test"}, WaitSec: 60, MaxRetries: 1}},
	}
	_ = s.CreateEscalationChain(ctx, "default", chain)

	_ = s.CreateDevice(ctx, "default", &store.Device{IMEI: "dev5"})
	dev, _ := s.GetDevice(ctx, "default", "dev5")
	dev.LastSeen = time.Now().Add(-2 * time.Hour)
	_ = s.UpdateDevice(ctx, "default", dev)

	m.Configure(Config{
		DeviceIMEI: "dev5",
		ChainID:    chain.ID,
		Interval:   1 * time.Hour,
		Grace:      10 * time.Minute,
		Enabled:    true,
	})

	m.scan(ctx)
	m.ClearAlert("dev5")
	m.scan(ctx)

	alerts, _ := s.ListAlerts(ctx, "default", false, 10)
	if len(alerts) != 2 {
		t.Errorf("expected 2 alerts after clear+retrigger, got %d", len(alerts))
	}
}

func TestConfigureDisabled_RemovesMonitoring(t *testing.T) {
	m := NewMonitor(nil, nil)
	m.Configure(Config{DeviceIMEI: "dev1", Enabled: true, Interval: time.Hour})
	if len(m.ListConfigs()) != 1 {
		t.Fatal("expected 1 config")
	}
	m.Configure(Config{DeviceIMEI: "dev1", Enabled: false})
	if len(m.ListConfigs()) != 0 {
		t.Fatal("expected 0 configs after disable")
	}
}

func TestRemove_CleansUp(t *testing.T) {
	m := NewMonitor(nil, nil)
	m.Configure(Config{DeviceIMEI: "dev1", Enabled: true, Interval: time.Hour})
	m.Snooze("dev1", time.Hour)
	m.Remove("dev1")
	if len(m.ListConfigs()) != 0 {
		t.Fatal("expected 0 configs after remove")
	}
}
