package ratelimit

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisLimiter implements per-device rate limiting using Redis.
// Shared across all Hub instances in cluster/k8s mode.
type RedisLimiter struct {
	client     *redis.Client
	dailyCap   int
	monthlyCap int
	prefix     string
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

// Allow checks if a send is permitted. Uses Redis INCR with daily/monthly key expiry.
// SOS messages always bypass.
func (l *RedisLimiter) Allow(deviceID string, isSOS bool) bool {
	if isSOS {
		return true
	}

	if isOverridden(deviceID) {
		return true
	}

	ctx := context.Background()

	// Monthly cap check
	if l.monthlyCap > 0 {
		month := time.Now().UTC().Format("2006-01")
		monthKey := fmt.Sprintf("%s%s:m:%s", l.prefix, deviceID, month)
		mCount, err := l.client.Get(ctx, monthKey).Int64()
		if err == nil && int(mCount) >= l.monthlyCap {
			slog.Warn("ratelimit: monthly cap exceeded", "device", deviceID, "count", mCount, "cap", l.monthlyCap)
			return false
		}
	}

	// Daily cap check + increment
	if l.dailyCap > 0 {
		today := time.Now().UTC().Format("2006-01-02")
		dayKey := fmt.Sprintf("%s%s:%s", l.prefix, deviceID, today)

		count, err := l.client.Incr(ctx, dayKey).Result()
		if err != nil {
			slog.Warn("ratelimit: redis incr error (fail-open)", "error", err, "device", deviceID)
			return true
		}
		if count == 1 {
			l.client.Expire(ctx, dayKey, 25*time.Hour)
		}
		if int(count) > l.dailyCap {
			slog.Warn("ratelimit: daily cap exceeded", "device", deviceID, "count", count, "cap", l.dailyCap)
			return false
		}
	}

	// Increment monthly counter
	if l.monthlyCap > 0 {
		month := time.Now().UTC().Format("2006-01")
		monthKey := fmt.Sprintf("%s%s:m:%s", l.prefix, deviceID, month)
		count, err := l.client.Incr(ctx, monthKey).Result()
		if err != nil {
			slog.Warn("ratelimit: redis monthly incr error", "error", err)
		}
		if count == 1 {
			l.client.Expire(ctx, monthKey, 32*24*time.Hour) // ~32 days TTL
		}
	}

	return true
}

// Usage returns current rate limit status for a device.
func (l *RedisLimiter) Usage(deviceID string) DeviceUsage {
	ctx := context.Background()
	today := time.Now().UTC().Format("2006-01-02")
	dayKey := fmt.Sprintf("%s%s:%s", l.prefix, deviceID, today)

	dailyCount := 0
	if v, err := l.client.Get(ctx, dayKey).Result(); err == nil {
		dailyCount, _ = strconv.Atoi(v)
	}

	monthlyCount := 0
	if l.monthlyCap > 0 {
		month := time.Now().UTC().Format("2006-01")
		monthKey := fmt.Sprintf("%s%s:m:%s", l.prefix, deviceID, month)
		if v, err := l.client.Get(ctx, monthKey).Result(); err == nil {
			monthlyCount, _ = strconv.Atoi(v)
		}
	}

	throttled := (l.dailyCap > 0 && dailyCount >= l.dailyCap) ||
		(l.monthlyCap > 0 && monthlyCount >= l.monthlyCap)

	usage := DeviceUsage{
		DeviceID:    deviceID,
		DailySent:   dailyCount,
		DailyCap:    l.dailyCap,
		MonthlySent: monthlyCount,
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

// AllUsage is not efficiently supported by Redis — returns empty.
// Use the /api/ratelimit/{deviceID} endpoint for per-device queries.
func (l *RedisLimiter) AllUsage() []DeviceUsage {
	return nil
}

// Compile-time check.
var _ Limiter = (*RedisLimiter)(nil)
