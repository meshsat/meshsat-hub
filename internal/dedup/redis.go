package dedup

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisDedup uses Redis SETNX with TTL for cross-instance deduplication.
// Used in cluster and k8s modes.
type RedisDedup struct {
	client *redis.Client
	ttl    time.Duration
	prefix string
}

// NewRedisDedup creates a new Redis-backed dedup tracker.
func NewRedisDedup(client *redis.Client, ttl time.Duration, prefix string) *RedisDedup {
	if prefix == "" {
		prefix = "dedup:"
	}
	return &RedisDedup{
		client: client,
		ttl:    ttl,
		prefix: prefix,
	}
}

// IsNew returns true if the key hasn't been seen within the TTL.
// Uses SETNX (SET if Not eXists) with expiry — atomic across all instances.
func (d *RedisDedup) IsNew(key string) bool {
	ctx := context.Background()
	ok, err := d.client.SetNX(ctx, d.prefix+key, "1", d.ttl).Result()
	if err != nil {
		// On Redis error, allow the message through (fail-open)
		return true
	}
	return ok // true = key was set (new), false = key already existed (duplicate)
}

// Compile-time check.
var _ Dedup = (*RedisDedup)(nil)
