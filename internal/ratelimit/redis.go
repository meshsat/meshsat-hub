package ratelimit

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/meshsat/meshsat-hub/internal/metrics"
	"github.com/redis/go-redis/v9"
)

// RedisLimiter implements per-device rate limiting using Redis.
// Shared across all Hub instances in cluster/k8s mode.
type RedisLimiter struct {
	client     *redis.Client
	dailyCap   int
	monthlyCap int
	prefix     string
	caps       CapResolver
}

// NewRedisLimiter creates a new Redis-backed rate limiter.
func NewRedisLimiter(client *redis.Client, dailyCap, monthlyCap int) *RedisLimiter {
	return &RedisLimiter{
		client:     client,
		dailyCap:   dailyCap,
		monthlyCap: monthlyCap,
		prefix:     "ratelimit:",
	}
}

// SetCapResolver makes the per-device budget depend on the tenant's plan and a
// platform admin's override, exactly as for DeviceLimiter.
func (l *RedisLimiter) SetCapResolver(r CapResolver) { l.caps = r }

// capsFor resolves the tenant's budget, falling back to the platform values.
// A resolver answering 0 means "nothing to say", never "unlimited".
func (l *RedisLimiter) capsFor(tenantID string) Caps {
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

// Allow checks if a send is permitted. Uses Redis INCR with daily/monthly key expiry.
// SOS messages always bypass.
func (l *RedisLimiter) Allow(tenantID, deviceID string, isSOS bool) bool {
	if isSOS {
		metrics.RatelimitDecisions.WithLabelValues("allowed").Inc()
		return true
	}

	if isOverridden(tenantID, deviceID) {
		metrics.RatelimitDecisions.WithLabelValues("allowed").Inc()
		return true
	}

	ctx := context.Background()
	caps := l.capsFor(tenantID)

	// Monthly cap check
	if caps.Monthly > 0 {
		month := time.Now().UTC().Format("2006-01")
		monthKey := fmt.Sprintf("%s%s:m:%s", l.prefix, scope(tenantID, deviceID), month)
		mCount, err := l.client.Get(ctx, monthKey).Int64()
		if err == nil && int(mCount) >= caps.Monthly {
			slog.Warn("ratelimit: monthly cap exceeded", "tenant", tenantID, "device", deviceID, "count", mCount, "cap", caps.Monthly)
			metrics.RatelimitDecisions.WithLabelValues("denied").Inc()
			metrics.RatelimitViolations.WithLabelValues("monthly_cap").Inc()
			return false
		}
	}

	// Daily cap check + increment
	if caps.Daily > 0 {
		today := time.Now().UTC().Format("2006-01-02")
		dayKey := fmt.Sprintf("%s%s:%s", l.prefix, scope(tenantID, deviceID), today)

		count, err := l.client.Incr(ctx, dayKey).Result()
		if err != nil {
			slog.Warn("ratelimit: redis incr error (fail-open)", "error", err, "device", deviceID)
			metrics.RatelimitDecisions.WithLabelValues("allowed").Inc()
			return true
		}
		if count == 1 {
			l.client.Expire(ctx, dayKey, 25*time.Hour)
		}
		if int(count) > caps.Daily {
			slog.Warn("ratelimit: daily cap exceeded", "tenant", tenantID, "device", deviceID, "count", count, "cap", caps.Daily)
			metrics.RatelimitDecisions.WithLabelValues("denied").Inc()
			metrics.RatelimitViolations.WithLabelValues("daily_cap").Inc()
			return false
		}
	}

	// Increment monthly counter
	if caps.Monthly > 0 {
		month := time.Now().UTC().Format("2006-01")
		monthKey := fmt.Sprintf("%s%s:m:%s", l.prefix, scope(tenantID, deviceID), month)
		count, err := l.client.Incr(ctx, monthKey).Result()
		if err != nil {
			slog.Warn("ratelimit: redis monthly incr error", "error", err)
		}
		if count == 1 {
			l.client.Expire(ctx, monthKey, 32*24*time.Hour) // ~32 days TTL
		}
	}

	metrics.RatelimitDecisions.WithLabelValues("allowed").Inc()
	return true
}

// Usage returns current rate limit status for a device.
func (l *RedisLimiter) Usage(tenantID, deviceID string) DeviceUsage {
	ctx := context.Background()
	caps := l.capsFor(tenantID)
	today := time.Now().UTC().Format("2006-01-02")
	dayKey := fmt.Sprintf("%s%s:%s", l.prefix, scope(tenantID, deviceID), today)

	dailyCount := 0
	if v, err := l.client.Get(ctx, dayKey).Result(); err == nil {
		dailyCount, _ = strconv.Atoi(v)
	}

	monthlyCount := 0
	if caps.Monthly > 0 {
		month := time.Now().UTC().Format("2006-01")
		monthKey := fmt.Sprintf("%s%s:m:%s", l.prefix, scope(tenantID, deviceID), month)
		if v, err := l.client.Get(ctx, monthKey).Result(); err == nil {
			monthlyCount, _ = strconv.Atoi(v)
		}
	}

	throttled := (caps.Daily > 0 && dailyCount >= caps.Daily) ||
		(caps.Monthly > 0 && monthlyCount >= caps.Monthly)

	usage := DeviceUsage{
		DeviceID:    deviceID,
		DailySent:   dailyCount,
		DailyCap:    caps.Daily,
		MonthlySent: monthlyCount,
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

// AllUsage is not efficiently supported by Redis — returns empty.
// Use the /api/ratelimit/{deviceID} endpoint for per-device queries.
func (l *RedisLimiter) AllUsage(string) []DeviceUsage {
	return nil
}

// Compile-time check.
var _ Limiter = (*RedisLimiter)(nil)
