package ratelimit

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/bus"
	"github.com/cubeos-app/meshsat-hub/internal/metrics"
)

// DeviceLimiter implements per-device token bucket rate limiting for MT satellite sends.
// SOS messages always bypass rate limiting.
type DeviceLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*tokenBucket
	maxTokens  float64
	refillRate float64 // tokens per second
	dailyCap   int     // max sends per device per day (0 = unlimited)
	daily      map[string]*dailyCounter
	monthlyCap int // max sends per device per month (0 = unlimited)
	monthly    map[string]*monthlyCounter
	mqtt       bus.MessageBus // for alert notifications
}

type tokenBucket struct {
	tokens     float64
	maxTokens  float64
	refillRate float64 // tokens per second
	lastRefill time.Time
}

type dailyCounter struct {
	count int
	date  string // "2006-01-02"
}

type monthlyCounter struct {
	count int
	month string // "2006-01"
}

// DeviceUsage reports current usage for a device.
type DeviceUsage struct {
	DeviceID      string  `json:"device_id"`
	TokensLeft    float64 `json:"tokens_left"`
	MaxTokens     float64 `json:"max_tokens"`
	DailySent     int     `json:"daily_sent"`
	DailyCap      int     `json:"daily_cap"`
	MonthlySent   int     `json:"monthly_sent"`
	MonthlyCap    int     `json:"monthly_cap"`
	Throttled     bool    `json:"throttled"`
	LastSend      string  `json:"last_send,omitempty"`
	OverrideUntil string  `json:"override_until,omitempty"`
}

// overrides stores admin override exemptions.
var overrides = struct {
	sync.RWMutex
	m map[string]time.Time // deviceID → override expiry
}{m: make(map[string]time.Time)}

// NewDeviceLimiter creates a new per-device rate limiter.
// maxTokens: burst capacity per device.
// refillRate: tokens refilled per second.
// dailyCap: max sends per device per 24h (0 = unlimited).
// monthlyCap: max sends per device per month (0 = unlimited).
func NewDeviceLimiter(maxTokens float64, refillRate float64, dailyCap, monthlyCap int, mqtt bus.MessageBus) *DeviceLimiter {
	return &DeviceLimiter{
		buckets:    make(map[string]*tokenBucket),
		maxTokens:  maxTokens,
		refillRate: refillRate,
		dailyCap:   dailyCap,
		daily:      make(map[string]*dailyCounter),
		monthlyCap: monthlyCap,
		monthly:    make(map[string]*monthlyCounter),
		mqtt:       mqtt,
	}
}

// Allow checks if a send is permitted for the given device.
// Returns true if allowed, false if rate-limited.
// isSOS=true always returns true (emergency bypass).
func (l *DeviceLimiter) Allow(deviceID string, isSOS bool) bool {
	if isSOS {
		slog.Debug("ratelimit: SOS bypass", "device", deviceID)
		metrics.RatelimitDecisions.WithLabelValues("allowed").Inc()
		return true
	}

	// Check admin override
	if isOverridden(deviceID) {
		metrics.RatelimitDecisions.WithLabelValues("allowed").Inc()
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// Monthly cap check
	if l.monthlyCap > 0 {
		mc := l.getMonthly(deviceID)
		month := time.Now().UTC().Format("2006-01")
		if mc.month != month {
			mc.count = 0
			mc.month = month
		}
		if mc.count >= l.monthlyCap {
			slog.Warn("ratelimit: monthly cap exceeded", "device", deviceID, "count", mc.count, "cap", l.monthlyCap)
			l.publishAlert(deviceID, "monthly_cap_exceeded", mc.count)
			metrics.RatelimitDecisions.WithLabelValues("denied").Inc()
			metrics.RatelimitViolations.WithLabelValues("monthly_cap").Inc()
			return false
		}
	}

	// Daily cap check
	if l.dailyCap > 0 {
		dc := l.getDaily(deviceID)
		today := time.Now().UTC().Format("2006-01-02")
		if dc.date != today {
			dc.count = 0
			dc.date = today
		}
		if dc.count >= l.dailyCap {
			slog.Warn("ratelimit: daily cap exceeded", "device", deviceID, "count", dc.count, "cap", l.dailyCap)
			l.publishAlert(deviceID, "daily_cap_exceeded", dc.count)
			metrics.RatelimitDecisions.WithLabelValues("denied").Inc()
			metrics.RatelimitViolations.WithLabelValues("daily_cap").Inc()
			return false
		}
	}

	// Token bucket check
	bucket := l.getBucket(deviceID)
	bucket.refill()
	if bucket.tokens < 1.0 {
		slog.Warn("ratelimit: throttled", "device", deviceID, "tokens", bucket.tokens)
		l.publishAlert(deviceID, "throttled", 0)
		metrics.RatelimitDecisions.WithLabelValues("denied").Inc()
		metrics.RatelimitViolations.WithLabelValues("throttled").Inc()
		return false
	}

	bucket.tokens -= 1.0

	// Increment counters
	if l.dailyCap > 0 {
		dc := l.getDaily(deviceID)
		dc.count++
	}
	if l.monthlyCap > 0 {
		mc := l.getMonthly(deviceID)
		mc.count++
	}

	metrics.RatelimitDecisions.WithLabelValues("allowed").Inc()
	return true
}

// Record records a successful send (for usage tracking after Allow returns true).
func (l *DeviceLimiter) Record(deviceID string) {
	// Already counted in Allow — this method exists for future metrics hooks.
}

// Usage returns current rate limit status for a device.
func (l *DeviceLimiter) Usage(deviceID string) DeviceUsage {
	l.mu.Lock()
	defer l.mu.Unlock()

	bucket := l.getBucket(deviceID)
	bucket.refill()

	dc := l.getDaily(deviceID)
	today := time.Now().UTC().Format("2006-01-02")
	if dc.date != today {
		dc.count = 0
		dc.date = today
	}

	mc := l.getMonthly(deviceID)
	month := time.Now().UTC().Format("2006-01")
	if mc.month != month {
		mc.count = 0
		mc.month = month
	}

	throttled := bucket.tokens < 1.0 ||
		(l.dailyCap > 0 && dc.count >= l.dailyCap) ||
		(l.monthlyCap > 0 && mc.count >= l.monthlyCap)

	usage := DeviceUsage{
		DeviceID:    deviceID,
		TokensLeft:  bucket.tokens,
		MaxTokens:   bucket.maxTokens,
		DailySent:   dc.count,
		DailyCap:    l.dailyCap,
		MonthlySent: mc.count,
		MonthlyCap:  l.monthlyCap,
		Throttled:   throttled,
	}

	overrides.RLock()
	if exp, ok := overrides.m[deviceID]; ok && time.Now().Before(exp) {
		usage.OverrideUntil = exp.Format(time.RFC3339)
	}
	overrides.RUnlock()

	return usage
}

// AllUsage returns usage for all tracked devices.
func (l *DeviceLimiter) AllUsage() []DeviceUsage {
	l.mu.Lock()
	defer l.mu.Unlock()

	var result []DeviceUsage
	today := time.Now().UTC().Format("2006-01-02")
	month := time.Now().UTC().Format("2006-01")
	for id, bucket := range l.buckets {
		bucket.refill()
		dc := l.getDaily(id)
		if dc.date != today {
			dc.count = 0
			dc.date = today
		}
		mc := l.getMonthly(id)
		if mc.month != month {
			mc.count = 0
			mc.month = month
		}
		throttled := bucket.tokens < 1.0 ||
			(l.dailyCap > 0 && dc.count >= l.dailyCap) ||
			(l.monthlyCap > 0 && mc.count >= l.monthlyCap)
		result = append(result, DeviceUsage{
			DeviceID:    id,
			TokensLeft:  bucket.tokens,
			MaxTokens:   bucket.maxTokens,
			DailySent:   dc.count,
			DailyCap:    l.dailyCap,
			MonthlySent: mc.count,
			MonthlyCap:  l.monthlyCap,
			Throttled:   throttled,
		})
	}
	return result
}

// SetOverride grants a temporary rate limit exemption for a device.
func SetOverride(deviceID string, duration time.Duration) {
	overrides.Lock()
	overrides.m[deviceID] = time.Now().Add(duration)
	overrides.Unlock()
	metrics.RatelimitOverridesActive.Inc()
	slog.Info("ratelimit: override set", "device", deviceID, "duration", duration)
}

// ClearOverride removes a rate limit exemption.
func ClearOverride(deviceID string) {
	overrides.Lock()
	_, existed := overrides.m[deviceID]
	delete(overrides.m, deviceID)
	overrides.Unlock()
	if existed {
		metrics.RatelimitOverridesActive.Dec()
	}
}

func isOverridden(deviceID string) bool {
	overrides.RLock()
	defer overrides.RUnlock()
	exp, ok := overrides.m[deviceID]
	return ok && time.Now().Before(exp)
}

func (l *DeviceLimiter) getBucket(deviceID string) *tokenBucket {
	b, ok := l.buckets[deviceID]
	if !ok {
		b = &tokenBucket{
			tokens:     l.maxTokens,
			maxTokens:  l.maxTokens,
			refillRate: l.refillRate,
			lastRefill: time.Now(),
		}
		l.buckets[deviceID] = b
	}
	return b
}

func (l *DeviceLimiter) getDaily(deviceID string) *dailyCounter {
	dc, ok := l.daily[deviceID]
	if !ok {
		dc = &dailyCounter{date: time.Now().UTC().Format("2006-01-02")}
		l.daily[deviceID] = dc
	}
	return dc
}

func (l *DeviceLimiter) getMonthly(deviceID string) *monthlyCounter {
	mc, ok := l.monthly[deviceID]
	if !ok {
		mc = &monthlyCounter{month: time.Now().UTC().Format("2006-01")}
		l.monthly[deviceID] = mc
	}
	return mc
}

func (b *tokenBucket) refill() {
	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens += elapsed * b.refillRate
	if b.tokens > b.maxTokens {
		b.tokens = b.maxTokens
	}
	b.lastRefill = now
}

func (l *DeviceLimiter) publishAlert(deviceID, reason string, count int) {
	if l.mqtt == nil {
		return
	}
	msg := map[string]interface{}{
		"device":    deviceID,
		"reason":    reason,
		"count":     count,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}
	data, _ := json.Marshal(msg)
	topic := "meshsat/hub/events"
	if err := l.mqtt.Publish(topic, 1, false, data); err != nil {
		slog.Warn("ratelimit: publish alert failed", "error", err)
	}
}

// Compile-time check.
var _ Limiter = (*DeviceLimiter)(nil)
