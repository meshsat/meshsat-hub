// Package deadman implements a per-device dead man's switch.
// Each device has a configurable check-in window. If the device does not
// send an MO message within that window + grace period, an alert is
// triggered via the escalation engine.
//
// State (configs, snoozes, the "already alerted" flag) is persisted in the
// store so every replica and every restart sees the same switch; the scan
// itself is meant to run on the leader only (MESHSAT-910).
package deadman

import (
	"context"
	"log/slog"
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
	tenantID string
}

// NewMonitor creates a dead man's switch monitor.
func NewMonitor(s store.Store, e *escalation.Engine) *Monitor {
	return &Monitor{
		store:    s,
		engine:   e,
		interval: 30 * time.Second,
		tenantID: store.DefaultTenantID,
	}
}

func (m *Monitor) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func toStored(cfg Config) *store.DeadmanConfig {
	return &store.DeadmanConfig{
		DeviceIMEI:  cfg.DeviceIMEI,
		ChainID:     cfg.ChainID,
		IntervalSec: int(cfg.Interval / time.Second),
		GraceSec:    int(cfg.Grace / time.Second),
		Enabled:     cfg.Enabled,
	}
}

func fromStored(c store.DeadmanConfig) Config {
	return Config{
		DeviceIMEI: c.DeviceIMEI,
		ChainID:    c.ChainID,
		Interval:   time.Duration(c.IntervalSec) * time.Second,
		Grace:      time.Duration(c.GraceSec) * time.Second,
		Enabled:    c.Enabled,
	}
}

// Configure sets the dead man's switch config for a device. Disabling
// removes it.
func (m *Monitor) Configure(cfg Config) {
	ctx, cancel := m.ctx()
	defer cancel()
	if !cfg.Enabled {
		if err := m.store.DeleteDeadmanConfig(ctx, m.tenantID, cfg.DeviceIMEI); err != nil {
			slog.Error("deadman: delete config", "device", cfg.DeviceIMEI, "error", err)
		}
		slog.Info("deadman: disabled", "device", cfg.DeviceIMEI)
		return
	}
	stored := toStored(cfg)
	// Keep snooze/alerted state across a reconfigure.
	if prev, err := m.store.GetDeadmanConfig(ctx, m.tenantID, cfg.DeviceIMEI); err == nil {
		stored.SnoozedUntil, stored.Alerted = prev.SnoozedUntil, prev.Alerted
	}
	if err := m.store.SaveDeadmanConfig(ctx, m.tenantID, stored); err != nil {
		slog.Error("deadman: save config", "device", cfg.DeviceIMEI, "error", err)
		return
	}
	slog.Info("deadman: configured",
		"device", cfg.DeviceIMEI, "interval", cfg.Interval, "grace", cfg.Grace, "enabled", cfg.Enabled)
}

// Remove disables the dead man's switch for a device.
func (m *Monitor) Remove(deviceIMEI string) {
	ctx, cancel := m.ctx()
	defer cancel()
	if err := m.store.DeleteDeadmanConfig(ctx, m.tenantID, deviceIMEI); err != nil {
		slog.Error("deadman: remove config", "device", deviceIMEI, "error", err)
	}
	slog.Info("deadman: removed", "device", deviceIMEI)
}

// Snooze temporarily suppresses the dead man's switch for a device.
func (m *Monitor) Snooze(deviceIMEI string, duration time.Duration) {
	m.update(deviceIMEI, func(c *store.DeadmanConfig) { c.SnoozedUntil = time.Now().UTC().Add(duration) })
	slog.Info("deadman: snoozed", "device", deviceIMEI, "duration", duration)
}

// ClearSnooze removes an active snooze.
func (m *Monitor) ClearSnooze(deviceIMEI string) {
	m.update(deviceIMEI, func(c *store.DeadmanConfig) { c.SnoozedUntil = time.Time{} })
}

// ClearAlert resets the alert state so the device can trigger again.
func (m *Monitor) ClearAlert(deviceIMEI string) {
	m.update(deviceIMEI, func(c *store.DeadmanConfig) { c.Alerted = false })
}

// update applies fn to the stored config of a device, if it exists.
func (m *Monitor) update(deviceIMEI string, fn func(*store.DeadmanConfig)) {
	ctx, cancel := m.ctx()
	defer cancel()
	c, err := m.store.GetDeadmanConfig(ctx, m.tenantID, deviceIMEI)
	if err != nil {
		return
	}
	fn(c)
	if err := m.store.SaveDeadmanConfig(ctx, m.tenantID, c); err != nil {
		slog.Error("deadman: update config", "device", deviceIMEI, "error", err)
	}
}

// CheckIn records that a device has sent a message (resets the window).
func (m *Monitor) CheckIn(deviceIMEI string) {
	ctx, cancel := m.ctx()
	defer cancel()
	if c, err := m.store.GetDeadmanConfig(ctx, m.tenantID, deviceIMEI); err == nil && c.Alerted {
		c.Alerted = false
		if err := m.store.SaveDeadmanConfig(ctx, m.tenantID, c); err == nil {
			slog.Info("deadman: device checked in, alert cleared", "device", deviceIMEI)
		}
	}
	// Touch last_seen in the store.
	_ = m.store.TouchDeviceLastSeen(ctx, m.tenantID, deviceIMEI)
}

// ListConfigs returns all active dead man's switch configs.
func (m *Monitor) ListConfigs() []Config {
	ctx, cancel := m.ctx()
	defer cancel()
	stored, err := m.store.ListDeadmanConfigs(ctx)
	if err != nil {
		slog.Error("deadman: list configs", "error", err)
		return nil
	}
	configs := make([]Config, 0, len(stored))
	for _, c := range stored {
		if c.Enabled {
			configs = append(configs, fromStored(c))
		}
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
	configs, err := m.store.ListDeadmanConfigs(ctx)
	if err != nil {
		slog.Error("deadman: list configs", "error", err)
		return
	}
	now := time.Now().UTC()
	for i := range configs {
		if configs[i].Enabled {
			m.checkDevice(ctx, &configs[i], now)
		}
	}
}

func (m *Monitor) checkDevice(ctx context.Context, cfg *store.DeadmanConfig, now time.Time) {
	if !cfg.SnoozedUntil.IsZero() && now.Before(cfg.SnoozedUntil) {
		return
	}
	if cfg.Alerted {
		return
	}

	device, err := m.store.GetDevice(ctx, cfg.TenantID, cfg.DeviceIMEI)
	if err != nil {
		slog.Debug("deadman: device not found", "device", cfg.DeviceIMEI, "error", err)
		return
	}

	deadline := device.LastSeen.Add(time.Duration(cfg.IntervalSec) * time.Second).Add(time.Duration(cfg.GraceSec) * time.Second)
	if now.Before(deadline) {
		return // device checked in within the window
	}

	// Mark alerted first (persisted) so a second replica or a restart does
	// not trigger the same alert again.
	cfg.Alerted = true
	if err := m.store.SaveDeadmanConfig(ctx, cfg.TenantID, cfg); err != nil {
		slog.Error("deadman: persist alerted flag", "device", cfg.DeviceIMEI, "error", err)
		return
	}

	alert := &store.Alert{
		ChainID:    cfg.ChainID,
		DeviceIMEI: cfg.DeviceIMEI,
		Type:       "deadman",
		Detail:     "Device missed check-in. Last seen: " + device.LastSeen.Format(time.RFC3339),
	}
	if err := m.engine.Trigger(ctx, cfg.TenantID, alert); err != nil {
		slog.Error("deadman: trigger alert failed", "device", cfg.DeviceIMEI, "error", err)
		return
	}

	slog.Warn("deadman: alert triggered",
		"device", cfg.DeviceIMEI, "last_seen", device.LastSeen, "deadline", deadline)
}
