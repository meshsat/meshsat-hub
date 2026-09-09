package tenancy

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bus"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// StatusTopic carries "this tenant's status changed" between replicas. The
// cache exists so the middleware is not a query per request, and the
// consequence is that a replica which did not handle the change keeps serving
// the old answer until its entry expires. For a closure that is untidy; for a
// suspension, which is what you reach for when a tenant is doing something you
// want stopped, waiting out a TTL is the wrong behaviour. So the replica that
// makes the change says so, and the others drop their entry and re-read.
const StatusTopic = "meshsat/hub/tenant/status"

type statusEvent struct {
	TenantID string `json:"tenant_id"`
	Status   string `json:"status"`
}

// StatusCache answers "is this tenant allowed to do anything" for the auth
// middleware, which asks on every request. A short TTL keeps that from being a
// database round trip per call while still making a suspension take effect in
// seconds rather than at the next restart.
//
// It caches the parts of the tenant row that gate a request -- status and plan
// -- from a single read, because both are wanted on the same paths and a plan
// change already invalidates the entry through the same topic a status change
// does.
type StatusCache struct {
	store store.Store
	ttl   time.Duration

	mu   sync.Mutex
	seen map[string]statusEntry
	bus  bus.MessageBus
}

// WithBus makes changes propagate to the other replicas.
func (c *StatusCache) WithBus(b bus.MessageBus) *StatusCache {
	c.bus = b
	return c
}

// Subscribe listens for changes made by another replica. Safe to call with no
// bus; then the TTL is the only thing keeping entries fresh.
func (c *StatusCache) Subscribe() error {
	if c == nil || c.bus == nil {
		return nil
	}
	return c.bus.Subscribe(StatusTopic, 1, func(_ string, payload []byte) {
		var ev statusEvent
		if err := json.Unmarshal(payload, &ev); err != nil || ev.TenantID == "" {
			return
		}
		c.forgetLocal(ev.TenantID)
		slog.Info("tenant status changed elsewhere", "tenant", ev.TenantID, "status", ev.Status)
	})
}

type statusEntry struct {
	status string
	plan   string
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
	e, err := c.entry(ctx, tenantID)
	return e.status, err
}

// Plan reports the tenant's subscription plan, from the same cached read the
// status comes from. An unknown tenant, or a read that fails, reports the empty
// string; the caller decides what that means. The quota checker treats it as
// the free tier, which is the safe direction: a tenant we cannot identify does
// not get an unlimited fleet.
func (c *StatusCache) Plan(ctx context.Context, tenantID string) (string, error) {
	e, err := c.entry(ctx, tenantID)
	return e.plan, err
}

func (c *StatusCache) entry(ctx context.Context, tenantID string) (statusEntry, error) {
	if c == nil || c.store == nil || tenantID == "" {
		return statusEntry{status: store.TenantActive}, nil
	}
	if c.ttl > 0 {
		c.mu.Lock()
		e, ok := c.seen[tenantID]
		c.mu.Unlock()
		if ok && time.Since(e.at) < c.ttl {
			return e, nil
		}
	}
	t, err := c.store.GetTenant(ctx, tenantID)
	if err != nil {
		return statusEntry{status: store.TenantActive}, err
	}
	e := statusEntry{status: t.Status, plan: t.Plan, at: time.Now()}
	if e.status == "" {
		e.status = store.TenantActive
	}
	if c.ttl > 0 {
		c.mu.Lock()
		c.seen[tenantID] = e
		c.mu.Unlock()
	}
	return e, nil
}

// Forget drops a cached status here and tells the other replicas to do the
// same, so a suspension or a closure applies to the next request anywhere
// rather than at the end of a TTL somewhere else.
func (c *StatusCache) Forget(tenantID string) {
	if c == nil {
		return
	}
	c.forgetLocal(tenantID)
	if c.bus == nil {
		return
	}
	if err := c.bus.PublishJSON(StatusTopic, 1, false, statusEvent{TenantID: tenantID}); err != nil {
		// The TTL still bounds how long a stale answer can survive, so this is
		// a delay, not a hole.
		slog.Warn("tenant status change not announced to the other replicas", "tenant", tenantID, "error", err)
	}
}

func (c *StatusCache) forgetLocal(tenantID string) {
	c.mu.Lock()
	delete(c.seen, tenantID)
	c.mu.Unlock()
}
