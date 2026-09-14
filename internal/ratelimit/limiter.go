package ratelimit

import (
	"encoding/json"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/metrics"
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
	// caps resolves a tenant's budget (MESHSAT-1117 tranche 2c). nil means
	// every tenant gets dailyCap/monthlyCap, which is what a self-hosted
	// single-tenant Hub has and what this was before.
	caps CapResolver
}

// SetCapResolver makes the per-device budget depend on the tenant's plan and
// on a platform admin's override. Called once at startup.
func (l *DeviceLimiter) SetCapResolver(r CapResolver) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.caps = r
}

// capsFor resolves the tenant's budget, falling back to the platform values.
// A resolver that answers 0 for either half means "nothing to say", not
// "unlimited" -- returning unlimited on a failed lookup would let a database
// blip hand somebody an unmetered satellite account.
//
// The caller must hold l.mu.
func (l *DeviceLimiter) capsFor(tenantID string) Caps {
	c := Caps{Daily: l.dailyCap, Monthly: l.monthlyCap}
	if l.caps == nil {
		return c
	}
	got := l.caps(tenantID)
	if got.Daily > 0 {
		c.Daily = got.Daily
	}
	if got.Monthly > 0 {
		c.Monthly = got.Monthly
	}
	return c
}

type tokenBucket struct {
	// tenantID and deviceID are carried on the bucket rather than parsed back
	// out of the map key, so AllUsage can filter to one tenant without having
	// to unescape anything.
	tenantID   string
	deviceID   string
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

// scope is the key every per-device structure in this package is stored under.
//
// MESHSAT-1118: everything here used to be keyed on the device id alone. The
// override map is the sharp end -- POST /api/ratelimit/{deviceID}/override is
// owner-gated but was not tenant-scoped, so the owner of ANY tenant could
// exempt ANOTHER tenant's device from the send budget, and the airtime that
// device then burns is billed to the victim's own Cloudloop or Twilio account.
// The counters were shared the same way, so two tenants' devices sharing an id
// spent one budget between them.
//
// Both halves are escaped because a device id is free-form text arriving from a
// URL path and an MQTT topic: without escaping, tenant "a" device "b:c" and
// tenant "a:b" device "c" would be the same key.
func scope(tenantID, deviceID string) string {
	return url.QueryEscape(tenantID) + ":" + url.QueryEscape(deviceID)
}

// overrides stores admin override exemptions, keyed by scope(tenant, device).
var overrides = struct {
	sync.RWMutex
	m map[string]time.Time // scope → override expiry
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
func (l *DeviceLimiter) Allow(tenantID, deviceID string, isSOS bool) bool {
	if isSOS {
		slog.Debug("ratelimit: SOS bypass", "device", deviceID, "tenant", tenantID)
		metrics.RatelimitDecisions.WithLabelValues("allowed").Inc()
		return true
	}

	// Check admin override
	if isOverridden(tenantID, deviceID) {
		metrics.RatelimitDecisions.WithLabelValues("allowed").Inc()
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	caps := l.capsFor(tenantID)

	// Monthly cap check
	if caps.Monthly > 0 {
		mc := l.getMonthly(tenantID, deviceID)
		month := time.Now().UTC().Format("2006-01")
		if mc.month != month {
			mc.count = 0
			mc.month = month
		}
		if mc.count >= caps.Monthly {
			slog.Warn("ratelimit: monthly cap exceeded", "tenant", tenantID, "device", deviceID, "count", mc.count, "cap", caps.Monthly)
			l.publishAlert(tenantID, deviceID, "monthly_cap_exceeded", mc.count)
			metrics.RatelimitDecisions.WithLabelValues("denied").Inc()
			metrics.RatelimitViolations.WithLabelValues("monthly_cap").Inc()
			return false
		}
	}

	// Daily cap check
	if caps.Daily > 0 {
		dc := l.getDaily(tenantID, deviceID)
		today := time.Now().UTC().Format("2006-01-02")
		if dc.date != today {
			dc.count = 0
			dc.date = today
		}
		if dc.count >= caps.Daily {
			slog.Warn("ratelimit: daily cap exceeded", "tenant", tenantID, "device", deviceID, "count", dc.count, "cap", caps.Daily)
			l.publishAlert(tenantID, deviceID, "daily_cap_exceeded", dc.count)
			metrics.RatelimitDecisions.WithLabelValues("denied").Inc()
			metrics.RatelimitViolations.WithLabelValues("daily_cap").Inc()
			return false
		}
	}

	// Token bucket check
	bucket := l.getBucket(tenantID, deviceID)
	bucket.refill()
	if bucket.tokens < 1.0 {
		slog.Warn("ratelimit: throttled", "tenant", tenantID, "device", deviceID, "tokens", bucket.tokens)
		l.publishAlert(tenantID, deviceID, "throttled", 0)
		metrics.RatelimitDecisions.WithLabelValues("denied").Inc()
		metrics.RatelimitViolations.WithLabelValues("throttled").Inc()
		return false
	}

	bucket.tokens -= 1.0

	// Increment counters
	if caps.Daily > 0 {
		dc := l.getDaily(tenantID, deviceID)
		dc.count++
	}
	if caps.Monthly > 0 {
		mc := l.getMonthly(tenantID, deviceID)
		mc.count++
	}

	metrics.RatelimitDecisions.WithLabelValues("allowed").Inc()
	return true
}

// Record records a successful send (for usage tracking after Allow returns true).
func (l *DeviceLimiter) Record(tenantID, deviceID string) {
	// Already counted in Allow — this method exists for future metrics hooks.
}

// Usage returns current rate limit status for a device.
func (l *DeviceLimiter) Usage(tenantID, deviceID string) DeviceUsage {
	l.mu.Lock()
	defer l.mu.Unlock()

	bucket := l.getBucket(tenantID, deviceID)
	bucket.refill()

	dc := l.getDaily(tenantID, deviceID)
	today := time.Now().UTC().Format("2006-01-02")
	if dc.date != today {
		dc.count = 0
		dc.date = today
	}

	mc := l.getMonthly(tenantID, deviceID)
	month := time.Now().UTC().Format("2006-01")
	if mc.month != month {
		mc.count = 0
		mc.month = month
	}

	caps := l.capsFor(tenantID)
	throttled := bucket.tokens < 1.0 ||
		(caps.Daily > 0 && dc.count >= caps.Daily) ||
		(caps.Monthly > 0 && mc.count >= caps.Monthly)

	usage := DeviceUsage{
		DeviceID:    deviceID,
		TokensLeft:  bucket.tokens,
		MaxTokens:   bucket.maxTokens,
		DailySent:   dc.count,
		DailyCap:    caps.Daily,
		MonthlySent: mc.count,
		MonthlyCap:  caps.Monthly,
		Throttled:   throttled,
	}

	overrides.RLock()
	if exp, ok := overrides.m[scope(tenantID, deviceID)]; ok && time.Now().Before(exp) {
		usage.OverrideUntil = exp.Format(time.RFC3339)
	}
	overrides.RUnlock()

	return usage
}

// AllUsage returns usage for the tenant's OWN tracked devices.
//
// It used to return every device the Hub had ever rate-limited, for every
// tenant: the id of each one and how much satellite traffic it is sending.
func (l *DeviceLimiter) AllUsage(tenantID string) []DeviceUsage {
	l.mu.Lock()
	defer l.mu.Unlock()

	caps := l.capsFor(tenantID)
	var result []DeviceUsage
	today := time.Now().UTC().Format("2006-01-02")
	month := time.Now().UTC().Format("2006-01")
	for _, bucket := range l.buckets {
		if bucket.tenantID != tenantID {
			continue
		}
		id := bucket.deviceID
		bucket.refill()
		dc := l.getDaily(tenantID, id)
		if dc.date != today {
			dc.count = 0
			dc.date = today
		}
		mc := l.getMonthly(tenantID, id)
		if mc.month != month {
			mc.count = 0
			mc.month = month
		}
		throttled := bucket.tokens < 1.0 ||
			(caps.Daily > 0 && dc.count >= caps.Daily) ||
			(caps.Monthly > 0 && mc.count >= caps.Monthly)
		result = append(result, DeviceUsage{
			DeviceID:    id,
			TokensLeft:  bucket.tokens,
			MaxTokens:   bucket.maxTokens,
			DailySent:   dc.count,
			DailyCap:    caps.Daily,
			MonthlySent: mc.count,
			MonthlyCap:  caps.Monthly,
			Throttled:   throttled,
		})
	}
	return result
}

// SetOverride grants a temporary rate limit exemption for one tenant's device.
//
// The exemption is recorded against the tenant that asked for it. The send path
// looks it up under the tenant that OWNS the device, so an override set by
// anyone else simply never matches -- which is the isolation, and it needs no
// database lookup on the message path to enforce.
func SetOverride(tenantID, deviceID string, duration time.Duration) {
	overrides.Lock()
	overrides.m[scope(tenantID, deviceID)] = time.Now().Add(duration)
	overrides.Unlock()
	metrics.RatelimitOverridesActive.Inc()
	slog.Info("ratelimit: override set", "tenant", tenantID, "device", deviceID, "duration", duration)
}

// ClearOverride removes one tenant's rate limit exemption.
func ClearOverride(tenantID, deviceID string) {
	k := scope(tenantID, deviceID)
	overrides.Lock()
	_, existed := overrides.m[k]
	delete(overrides.m, k)
	overrides.Unlock()
	if existed {
		metrics.RatelimitOverridesActive.Dec()
	}
}

func isOverridden(tenantID, deviceID string) bool {
	overrides.RLock()
	defer overrides.RUnlock()
	exp, ok := overrides.m[scope(tenantID, deviceID)]
	return ok && time.Now().Before(exp)
}

func (l *DeviceLimiter) getBucket(tenantID, deviceID string) *tokenBucket {
	k := scope(tenantID, deviceID)
	b, ok := l.buckets[k]
	if !ok {
		b = &tokenBucket{
			tenantID:   tenantID,
			deviceID:   deviceID,
			tokens:     l.maxTokens,
			maxTokens:  l.maxTokens,
			refillRate: l.refillRate,
			lastRefill: time.Now(),
		}
		l.buckets[k] = b
	}
	return b
}

func (l *DeviceLimiter) getDaily(tenantID, deviceID string) *dailyCounter {
	k := scope(tenantID, deviceID)
	dc, ok := l.daily[k]
	if !ok {
		dc = &dailyCounter{date: time.Now().UTC().Format("2006-01-02")}
		l.daily[k] = dc
	}
	return dc
}

func (l *DeviceLimiter) getMonthly(tenantID, deviceID string) *monthlyCounter {
	k := scope(tenantID, deviceID)
	mc, ok := l.monthly[k]
	if !ok {
		mc = &monthlyCounter{month: time.Now().UTC().Format("2006-01")}
		l.monthly[k] = mc
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

func (l *DeviceLimiter) publishAlert(tenantID, deviceID, reason string, count int) {
	if l.mqtt == nil {
		return
	}
	msg := map[string]interface{}{
		"tenant":    tenantID,
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
