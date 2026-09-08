package tenancy

import (
	"context"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// StatusCache answers "is this tenant allowed to do anything" for the auth
// middleware, which asks on every request. A short TTL keeps that from being a
// database round trip per call while still making a suspension take effect in
// seconds rather than at the next restart.
type StatusCache struct {
	store store.Store
	ttl   time.Duration

	mu   sync.Mutex
	seen map[string]statusEntry
}

type statusEntry struct {
	status string
	at     time.Time
}

// NewStatusCache returns a cache over the store. ttl of 0 disables caching.
func NewStatusCache(s store.Store, ttl time.Duration) *StatusCache {
	return &StatusCache{store: s, ttl: ttl, seen: map[string]statusEntry{}}
}

// Status reports the tenant's lifecycle status. An unknown tenant reports
// active: it is not this cache's job to invent an authorisation failure for a
// tenant the rest of the system has not heard of.
func (c *StatusCache) Status(ctx context.Context, tenantID string) (string, error) {
	if c == nil || c.store == nil || tenantID == "" {
		return store.TenantActive, nil
	}
	if c.ttl > 0 {
		c.mu.Lock()
		e, ok := c.seen[tenantID]
		c.mu.Unlock()
		if ok && time.Since(e.at) < c.ttl {
			return e.status, nil
		}
	}
	t, err := c.store.GetTenant(ctx, tenantID)
	if err != nil {
		return store.TenantActive, err
	}
	st := t.Status
	if st == "" {
		st = store.TenantActive
	}
	if c.ttl > 0 {
		c.mu.Lock()
		c.seen[tenantID] = statusEntry{status: st, at: time.Now()}
		c.mu.Unlock()
	}
	return st, nil
}

// Forget drops a cached status so a suspension or restoration applies now
// rather than at the end of the TTL.
func (c *StatusCache) Forget(tenantID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.seen, tenantID)
	c.mu.Unlock()
}
