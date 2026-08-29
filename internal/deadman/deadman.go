// Package deadman implements a per-device dead man's switch.
// Each device has a configurable check-in window. If the device does not
// send an MO message within that window + grace period, an alert is
// triggered via the escalation engine.
package deadman

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/escalation"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// Config holds dead man's switch settings for a single device.
type Config struct {
	DeviceIMEI string        `json:"device_imei"`
	ChainID    string        `json:"chain_id"` // escalation chain to trigger
	Interval   time.Duration `json:"interval"` // expected check-in interval
	Grace      time.Duration `json:"grace"`    // grace period after interval expires
	Enabled    bool          `json:"enabled"`
}

// Monitor tracks device check-ins and triggers escalation on missed windows.
type Monitor struct {
	store    store.Store
	engine   *escalation.Engine
	interval time.Duration // how often to scan for missed check-ins

	mu      sync.Mutex
	configs map[string]*Config   // key: device IMEI
	snoozed map[string]time.Time // key: device IMEI, value: snooze-until
	alerted map[string]bool      // key: device IMEI, true if alert already active
}

// NewMonitor creates a dead man's switch monitor.
func NewMonitor(s store.Store, e *escalation.Engine) *Monitor {
	return &Monitor{
		store:    s,
		engine:   e,
		interval: 30 * time.Second,
		configs:  make(map[string]*Config),
		snoozed:  make(map[string]time.Time),
		alerted:  make(map[string]bool),
	}
}

// Configure sets the dead man's switch config for a device.
func (m *Monitor) Configure(cfg Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cfg.Enabled {
		m.configs[cfg.DeviceIMEI] = &cfg
	} else {
		delete(m.configs, cfg.DeviceIMEI)
	}
	slog.Info("deadman: configured",
		"device", cfg.DeviceIMEI, "interval", cfg.Interval, "grace", cfg.Grace, "enabled", cfg.Enabled)
}

// Remove disables the dead man's switch for a device.
func (m *Monitor) Remove(deviceIMEI string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.configs, deviceIMEI)
	delete(m.snoozed, deviceIMEI)
	delete(m.alerted, deviceIMEI)
}

// Snooze temporarily suppresses the dead man's switch for a device.
func (m *Monitor) Snooze(deviceIMEI string, duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snoozed[deviceIMEI] = time.Now().Add(duration)
	slog.Info("deadman: snoozed", "device", deviceIMEI, "until", m.snoozed[deviceIMEI])
}

// ClearSnooze removes the snooze for a device.
func (m *Monitor) ClearSnooze(deviceIMEI string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.snoozed, deviceIMEI)
}

// ClearAlert resets the alert flag for a device (e.g., after device checks in again).
func (m *Monitor) ClearAlert(deviceIMEI string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.alerted, deviceIMEI)
}

// CheckIn records device activity: touches last_seen in the store and clears
// any active dead man's switch alert so the device can re-trigger if it goes
// silent again. Call this from position subscriber and MO handler.
func (m *Monitor) CheckIn(deviceIMEI string) {
	// Clear alert so device can re-trigger on next silence.
	m.mu.Lock()
	wasAlerted := m.alerted[deviceIMEI]
	delete(m.alerted, deviceIMEI)
	m.mu.Unlock()

	if wasAlerted {
		slog.Info("deadman: device checked in, alert cleared", "device", deviceIMEI)
	}

	// Touch last_seen in the store.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = m.store.TouchDeviceLastSeen(ctx, store.DefaultTenantID, deviceIMEI)
}

// ListConfigs returns all active dead man's switch configs.
func (m *Monitor) ListConfigs() []Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	configs := make([]Config, 0, len(m.configs))
	for _, c := range m.configs {
		configs = append(configs, *c)
	}
	return configs
}

// Start begins the dead man's switch monitoring loop. Blocks until ctx is cancelled.
func (m *Monitor) Start(ctx context.Context) {
	slog.Info("deadman: monitor started", "scan_interval", m.interval)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("deadman: monitor stopped")
			return
		case <-ticker.C:
			m.scan(ctx)
		}
	}
}

func (m *Monitor) scan(ctx context.Context) {
	m.mu.Lock()
	configs := make([]*Config, 0, len(m.configs))
	for _, c := range m.configs {
		configs = append(configs, c)
	}
	m.mu.Unlock()

	now := time.Now().UTC()
	for _, cfg := range configs {
		m.checkDevice(ctx, cfg, now)
	}
}

func (m *Monitor) checkDevice(ctx context.Context, cfg *Config, now time.Time) {
	m.mu.Lock()
	// Check snooze.
	if until, ok := m.snoozed[cfg.DeviceIMEI]; ok && now.Before(until) {
		m.mu.Unlock()
		return
	}
	// Check if already alerted.
	if m.alerted[cfg.DeviceIMEI] {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	// Get device last_seen from store.
	// Use empty tenant for cross-tenant monitoring (dead man's switch is safety-critical).
	device, err := m.store.GetDevice(ctx, store.DefaultTenantID, cfg.DeviceIMEI)
	if err != nil {
		slog.Debug("deadman: device not found", "device", cfg.DeviceIMEI, "error", err)
		return
	}

	deadline := device.LastSeen.Add(cfg.Interval).Add(cfg.Grace)
	if now.Before(deadline) {
		return // device checked in within the window
	}

	// Device missed check-in — trigger alert.
	m.mu.Lock()
	m.alerted[cfg.DeviceIMEI] = true
	m.mu.Unlock()

	alert := &store.Alert{
		ChainID:    cfg.ChainID,
		DeviceIMEI: cfg.DeviceIMEI,
		Type:       "deadman",
		Detail:     "Device missed check-in. Last seen: " + device.LastSeen.Format(time.RFC3339),
	}
	if err := m.engine.Trigger(ctx, store.DefaultTenantID, alert); err != nil {
		slog.Error("deadman: trigger alert failed", "device", cfg.DeviceIMEI, "error", err)
		return
	}

	slog.Warn("deadman: alert triggered",
		"device", cfg.DeviceIMEI, "last_seen", device.LastSeen, "deadline", deadline)
}
