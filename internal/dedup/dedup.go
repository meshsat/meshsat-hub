// Package dedup provides idempotent message processing via deduplication.
// Implementations: MemoryDedup (standalone) and RedisDedup (cluster/k8s).
package dedup

import (
	"sync"
	"time"
)

// Dedup checks whether a key has been seen before within a TTL window.
type Dedup interface {
	// IsNew returns true if the key has NOT been seen before within the TTL.
	// If true, the key is recorded. Subsequent calls with the same key
	// within TTL return false.
	IsNew(key string) bool
}

// MemoryDedup is an in-memory dedup tracker with TTL pruning.
// Used in standalone mode.
type MemoryDedup struct {
	mu   sync.Mutex
	seen map[string]time.Time
	ttl  time.Duration
	stop chan struct{}
}

// NewMemoryDedup creates a new in-memory dedup tracker.
func NewMemoryDedup(ttl time.Duration) *MemoryDedup {
	d := &MemoryDedup{
		seen: make(map[string]time.Time),
		ttl:  ttl,
		stop: make(chan struct{}),
	}
	go d.pruner()
	return d
}

// IsNew returns true if the key hasn't been seen within the TTL.
func (d *MemoryDedup) IsNew(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if exp, ok := d.seen[key]; ok && time.Now().Before(exp) {
		return false // already seen
	}
	d.seen[key] = time.Now().Add(d.ttl)
	return true
}

// Close stops the pruner goroutine.
func (d *MemoryDedup) Close() {
	close(d.stop)
}

func (d *MemoryDedup) pruner() {
	ticker := time.NewTicker(d.ttl / 2)
	defer ticker.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-ticker.C:
			d.mu.Lock()
			now := time.Now()
			for k, exp := range d.seen {
				if now.After(exp) {
					delete(d.seen, k)
				}
			}
			d.mu.Unlock()
		}
	}
}

// Compile-time check.
var _ Dedup = (*MemoryDedup)(nil)
