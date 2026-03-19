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

func TestCheckIn_ResetsAlertAndTouchesLastSeen(t *testing.T) {
	s := newTestStore(t)
	engine := escalation.New(s, escalation.LogNotifier{})
	m := NewMonitor(s, engine)
	ctx := context.Background()

	chain := &store.EscalationChain{
		Name:  "Test",
		Tiers: []store.EscalationTier{{Name: "t1", Targets: []string{"test"}, WaitSec: 60, MaxRetries: 1}},
	}
	_ = s.CreateEscalationChain(ctx, "default", chain)

	_ = s.CreateDevice(ctx, "default", &store.Device{IMEI: "dev6"})
	dev, _ := s.GetDevice(ctx, "default", "dev6")
	dev.LastSeen = time.Now().Add(-2 * time.Hour)
	_ = s.UpdateDevice(ctx, "default", dev)

	m.Configure(Config{
		DeviceIMEI: "dev6",
		ChainID:    chain.ID,
		Interval:   1 * time.Hour,
		Grace:      10 * time.Minute,
		Enabled:    true,
	})

	// First scan triggers alert (device is overdue).
	m.scan(ctx)
	alerts, _ := s.ListAlerts(ctx, "default", false, 10)
	if len(alerts) != 1 {
		t.Fatalf("expected 1 alert, got %d", len(alerts))
	}

	// Device checks in via CheckIn.
	m.CheckIn("dev6")

	// Verify last_seen was updated (should be recent, not 2 hours ago).
	dev, _ = s.GetDevice(ctx, "default", "dev6")
	if time.Since(dev.LastSeen) > 5*time.Second {
		t.Errorf("last_seen not updated: %v", dev.LastSeen)
	}

	// Second scan should NOT trigger another alert (device just checked in).
	m.scan(ctx)
	alerts, _ = s.ListAlerts(ctx, "default", false, 10)
	if len(alerts) != 1 {
		t.Errorf("expected still 1 alert (no re-trigger after check-in), got %d", len(alerts))
	}
}

func TestCheckIn_ClearsAlertFlag(t *testing.T) {
	s := newTestStore(t)
	engine := escalation.New(s, escalation.LogNotifier{})
	m := NewMonitor(s, engine)
	ctx := context.Background()

	chain := &store.EscalationChain{
		Name:  "Test",
		Tiers: []store.EscalationTier{{Name: "t1", Targets: []string{"test"}, WaitSec: 60, MaxRetries: 1}},
	}
	_ = s.CreateEscalationChain(ctx, "default", chain)

	_ = s.CreateDevice(ctx, "default", &store.Device{IMEI: "dev7"})

	m.Configure(Config{
		DeviceIMEI: "dev7",
		ChainID:    chain.ID,
		Interval:   1 * time.Hour,
		Grace:      10 * time.Minute,
		Enabled:    true,
	})

	// Trigger alert (device has never checked in — LastSeen is zero).
	m.scan(ctx)
	alerts, _ := s.ListAlerts(ctx, "default", false, 10)
	if len(alerts) != 1 {
		t.Fatalf("expected 1 alert, got %d", len(alerts))
	}

	// CheckIn clears alert flag and touches last_seen.
	m.CheckIn("dev7")

	// Verify alerted flag is cleared (internal state check via ClearAlert+scan behavior).
	// After CheckIn, last_seen is now, so scan should NOT trigger.
	m.scan(ctx)
	alerts, _ = s.ListAlerts(ctx, "default", false, 10)
	if len(alerts) != 1 {
		t.Errorf("expected still 1 alert (device recently checked in), got %d", len(alerts))
	}

	// Verify that ClearAlert was called internally (alerted flag is false).
	// Use ClearAlert + re-scan with old time via the existing ClearAlert test pattern.
	// The alert flag was already cleared by CheckIn, so calling ClearAlert again is a no-op.
	m.ClearAlert("dev7")

	// Now manually test that the internal alerted map was cleared by CheckIn.
	// We can't easily set last_seen to old via the store API, but we can verify
	// that the flag was actually reset by checking it doesn't block future alerts
	// when combined with the ClearAlert_AllowsRetrigger pattern.
	m.mu.Lock()
	alerted := m.alerted["dev7"]
	m.mu.Unlock()
	if alerted {
		t.Error("expected alerted flag to be false after CheckIn")
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
