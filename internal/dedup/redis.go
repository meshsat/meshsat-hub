package dedup

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisDedup uses Redis SET NX with TTL for cross-instance deduplication.
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
// Uses SET with NX option — atomic set-if-not-exists across all instances.
func (d *RedisDedup) IsNew(key string) bool {
	ctx := context.Background()
	// SET key value NX PX ttl — returns OK if set, nil if already exists
	result, err := d.client.Do(ctx, "SET", d.prefix+key, "1", "NX", "PX", d.ttl.Milliseconds()).Result()
	if err != nil {
		if err == redis.Nil {
			return false // key already exists
		}
		return true // fail-open on Redis error
	}
	return result == "OK"
}

// Compile-time check.
var _ Dedup = (*RedisDedup)(nil)
