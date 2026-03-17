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
	client   *redis.Client
	dailyCap int
	prefix   string
}

// NewRedisLimiter creates a new Redis-backed rate limiter.
func NewRedisLimiter(client *redis.Client, dailyCap int) *RedisLimiter {
	return &RedisLimiter{
		client:   client,
		dailyCap: dailyCap,
		prefix:   "ratelimit:",
	}
}

// Allow checks if a send is permitted. Uses Redis INCR with daily key expiry.
// SOS messages always bypass.
func (l *RedisLimiter) Allow(deviceID string, isSOS bool) bool {
	if isSOS {
		return true
	}

	// Check admin override
	if isOverridden(deviceID) {
		return true
	}

	if l.dailyCap <= 0 {
		return true // no cap configured
	}

	ctx := context.Background()
	today := time.Now().UTC().Format("2006-01-02")
	key := fmt.Sprintf("%s%s:%s", l.prefix, deviceID, today)

	count, err := l.client.Incr(ctx, key).Result()
	if err != nil {
		slog.Warn("ratelimit: redis incr error (fail-open)", "error", err, "device", deviceID)
		return true // fail-open on Redis error
	}

	// Set expiry on first increment (TTL = rest of day + 1h buffer)
	if count == 1 {
		l.client.Expire(ctx, key, 25*time.Hour)
	}

	if int(count) > l.dailyCap {
		slog.Warn("ratelimit: daily cap exceeded", "device", deviceID, "count", count, "cap", l.dailyCap)
		return false
	}

	return true
}

// Usage returns current rate limit status for a device.
func (l *RedisLimiter) Usage(deviceID string) DeviceUsage {
	ctx := context.Background()
	today := time.Now().UTC().Format("2006-01-02")
	key := fmt.Sprintf("%s%s:%s", l.prefix, deviceID, today)

	countStr, err := l.client.Get(ctx, key).Result()
	count := 0
	if err == nil {
		count, _ = strconv.Atoi(countStr)
	}

	usage := DeviceUsage{
		DeviceID:  deviceID,
		DailySent: count,
		DailyCap:  l.dailyCap,
		Throttled: l.dailyCap > 0 && count >= l.dailyCap,
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
