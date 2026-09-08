// Package tenancy resolves which tenant an inbound message belongs to.
//
// Field devices and bridges publish on tenant-less MQTT topics
// (meshsat/{device}/..., meshsat/bridge/{id}/...), so every subscriber and the
// routing engine used to assume the default tenant. The Resolver maps a device
// IMEI or a bridge ID to the tenant that owns it in the store, with a short
// cache, and falls back to the default tenant for devices nobody has
// registered yet (they auto-register there, as before).
package tenancy

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// lookupStore is the slice of store.Store the resolver needs.
type lookupStore interface {
	LookupDeviceTenant(ctx context.Context, imei string) (string, error)
	LookupBridgeTenant(ctx context.Context, bridgeID string) (string, error)
	GetTenant(ctx context.Context, id string) (*store.Tenant, error)
}

type entry struct {
	tenant string
	known  bool // the id is registered (tenant is not just the fallback)
	expiry time.Time
}

// Resolver maps devices and bridges to tenants.
type Resolver struct {
	store   lookupStore
	def     string
	ttl     time.Duration
	mu      sync.Mutex
	devices map[string]entry
	bridges map[string]entry
	tenants map[string]entry // tenant id → "1" when it exists
	now     func() time.Time
}

// NewResolver returns a resolver backed by s. Unknown devices and bridges
// resolve to defaultTenant. ttl bounds how long a lookup result (positive or
// negative) is reused; 0 disables caching.
func NewResolver(s lookupStore, defaultTenant string, ttl time.Duration) *Resolver {
	if defaultTenant == "" {
		defaultTenant = store.DefaultTenantID
	}
	return &Resolver{store: s, def: defaultTenant, ttl: ttl, devices: map[string]entry{}, bridges: map[string]entry{}, tenants: map[string]entry{}, now: time.Now}
}

// Default returns the fallback tenant.
func (r *Resolver) Default() string { return r.def }

// ForDevice returns the tenant owning the device with this IMEI, or the
// default tenant when it is unknown, ambiguous, or the store fails.
func (r *Resolver) ForDevice(ctx context.Context, imei string) string {
	return r.resolve(ctx, r.devices, imei, r.store.LookupDeviceTenant, "device")
}

// ForBridge returns the tenant owning the bridge, or the default tenant.
func (r *Resolver) ForBridge(ctx context.Context, bridgeID string) string {
	return r.resolve(ctx, r.bridges, bridgeID, r.store.LookupBridgeTenant, "bridge")
}

// ForDeviceTopic resolves the tenant of a message received on a device topic.
// The store is authoritative for registered devices (a publisher cannot move a
// device by using another tenant's prefix); an unregistered device is placed
// in the tenant named by the topic when that tenant exists, else the default.
func (r *Resolver) ForDeviceTopic(ctx context.Context, imei, topicTenant string) string {
	owner, known := r.known(ctx, r.devices, imei, r.store.LookupDeviceTenant, "device")
	return r.reconcile(ctx, owner, known, topicTenant, "device", imei)
}

// ForBridgeTopic is ForDeviceTopic for bridge topics.
func (r *Resolver) ForBridgeTopic(ctx context.Context, bridgeID, topicTenant string) string {
	owner, known := r.known(ctx, r.bridges, bridgeID, r.store.LookupBridgeTenant, "bridge")
	return r.reconcile(ctx, owner, known, topicTenant, "bridge", bridgeID)
}

func (r *Resolver) reconcile(ctx context.Context, owner string, known bool, topicTenant, kind, id string) string {
	if known {
		if topicTenant != "" && topicTenant != owner {
			slog.Warn("tenancy: topic tenant differs from the owner, using the owner", "kind", kind, "id", id, "topic_tenant", topicTenant, "owner", owner)
		}
		return owner
	}
	if topicTenant == "" || topicTenant == r.def {
		return r.def
	}
	if r.tenantExists(ctx, topicTenant) {
		return topicTenant
	}
	slog.Warn("tenancy: topic names an unknown tenant, using default", "kind", kind, "id", id, "topic_tenant", topicTenant)
	return r.def
}

// known returns the owning tenant and whether the id is registered at all.
func (r *Resolver) known(ctx context.Context, cache map[string]entry, key string, lookup func(context.Context, string) (string, error), kind string) (string, bool) {
	if key == "" || r.store == nil {
		return r.def, false
	}
	if r.ttl > 0 {
		r.mu.Lock()
		e, ok := cache[key]
		r.mu.Unlock()
		if ok && r.now().Before(e.expiry) {
			return e.tenant, e.tenant != "" && e.known
		}
	}
	tenant, err := lookup(ctx, key)
	found := err == nil && tenant != ""
	switch {
	case found:
	case errors.Is(err, store.ErrNotFound):
		tenant = r.def
	case errors.Is(err, store.ErrAmbiguousTenant):
		slog.Warn("tenancy: id present in several tenants, using default", "kind", kind, "id", key)
		tenant = r.def
	default:
		slog.Warn("tenancy: lookup failed, using default", "kind", kind, "id", key, "error", err)
		return r.def, false
	}
	if r.ttl > 0 {
		r.mu.Lock()
		cache[key] = entry{tenant: tenant, known: found, expiry: r.now().Add(r.ttl)}
		r.mu.Unlock()
	}
	return tenant, found
}

func (r *Resolver) tenantExists(ctx context.Context, id string) bool {
	if r.ttl > 0 {
		r.mu.Lock()
		e, ok := r.tenants[id]
		r.mu.Unlock()
		if ok && r.now().Before(e.expiry) {
			return e.known
		}
	}
	t, err := r.store.GetTenant(ctx, id)
	exists := err == nil && t != nil
	if r.ttl > 0 && (exists || errors.Is(err, store.ErrNotFound) || err == nil) {
		r.mu.Lock()
		r.tenants[id] = entry{known: exists, expiry: r.now().Add(r.ttl)}
		r.mu.Unlock()
	}
	return exists
}

// Forget drops cached answers for a device (call after moving or deleting it).
func (r *Resolver) Forget(imei string) {
	r.mu.Lock()
	delete(r.devices, imei)
	r.mu.Unlock()
}

func (r *Resolver) resolve(ctx context.Context, cache map[string]entry, key string, lookup func(context.Context, string) (string, error), kind string) string {
	tenant, _ := r.known(ctx, cache, key, lookup, kind)
	return tenant
}

type ctxKey struct{}

// WithTenant stores the resolved tenant in ctx for downstream handlers.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, ctxKey{}, tenantID)
}

// FromContext returns the tenant stored by WithTenant, or "" when absent.
func FromContext(ctx context.Context) string {
	t, _ := ctx.Value(ctxKey{}).(string)
	return t
}
