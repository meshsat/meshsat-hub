package relay

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Budget answers whether one more frame from a client may pass this minute.
// The key is relay:{tenant}:{client}; the S9-05 contract is 100 per minute
// per client, counted on the untrusted end (the client's frames and its
// connect), never on the bridge's replies.
type Budget interface {
	Allow(key string, limit int) bool
}

// MemoryBudget is a fixed-window counter per key, for a single replica. With
// two replicas each client gets the budget twice over; the Redis one is what
// production uses, this is the fallback when there is no Redis.
type MemoryBudget struct {
	mu     sync.Mutex
	now    func() time.Time
	window map[string]window
}

type window struct {
	minute int64
	count  int
}

// NewMemoryBudget returns an in-process budget.
func NewMemoryBudget() *MemoryBudget {
	return &MemoryBudget{now: time.Now, window: map[string]window{}}
}

// Allow implements Budget.
func (b *MemoryBudget) Allow(key string, limit int) bool {
	if limit <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	minute := b.now().Unix() / 60
	w := b.window[key]
	if w.minute != minute {
		// A new minute; this is also the only pruning, so the map holds at
		// most one entry per key that spoke in the current or previous minute.
		if len(b.window) > 4096 {
			for k, v := range b.window {
				if v.minute != minute {
					delete(b.window, k)
				}
			}
		}
		w = window{minute: minute}
	}
	w.count++
	b.window[key] = w
	return w.count <= limit
}

// RedisBudget shares the window across replicas: INCR on a key that names
// the minute, expiring two minutes later. A Redis error fails OPEN, like the
// send-path limiter: a frame delivered over budget is a nuisance, a tunnel
// closed because the cache blinked is an outage.
type RedisBudget struct {
	client *redis.Client
	prefix string
	now    func() time.Time
}

// NewRedisBudget returns a budget shared through client.
func NewRedisBudget(client *redis.Client) *RedisBudget {
	return &RedisBudget{client: client, prefix: "relay:", now: time.Now}
}

// Allow implements Budget.
func (b *RedisBudget) Allow(key string, limit int) bool {
	if limit <= 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	k := b.prefix + key + ":" + time.Unix(b.now().Unix()/60*60, 0).UTC().Format("200601021504")
	n, err := b.client.Incr(ctx, k).Result()
	if err != nil {
		slog.Warn("relay: budget incr failed (fail-open)", "error", err)
		return true
	}
	if n == 1 {
		b.client.Expire(ctx, k, 2*time.Minute)
	}
	return int(n) <= limit
}
