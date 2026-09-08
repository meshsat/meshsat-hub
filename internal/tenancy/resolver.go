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
}

type entry struct {
	tenant string
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
	now     func() time.Time
}

// NewResolver returns a resolver backed by s. Unknown devices and bridges
// resolve to defaultTenant. ttl bounds how long a lookup result (positive or
// negative) is reused; 0 disables caching.
func NewResolver(s lookupStore, defaultTenant string, ttl time.Duration) *Resolver {
	if defaultTenant == "" {
		defaultTenant = store.DefaultTenantID
	}
	return &Resolver{store: s, def: defaultTenant, ttl: ttl, devices: map[string]entry{}, bridges: map[string]entry{}, now: time.Now}
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

// Forget drops cached answers for a device (call after moving or deleting it).
func (r *Resolver) Forget(imei string) {
	r.mu.Lock()
	delete(r.devices, imei)
	r.mu.Unlock()
}

func (r *Resolver) resolve(ctx context.Context, cache map[string]entry, key string, lookup func(context.Context, string) (string, error), kind string) string {
	if key == "" || r.store == nil {
		return r.def
	}
	if r.ttl > 0 {
		r.mu.Lock()
		e, ok := cache[key]
		r.mu.Unlock()
		if ok && r.now().Before(e.expiry) {
			return e.tenant
		}
	}
	tenant, err := lookup(ctx, key)
	switch {
	case err == nil && tenant != "":
	case errors.Is(err, store.ErrNotFound):
		tenant = r.def
	case errors.Is(err, store.ErrAmbiguousTenant):
		slog.Warn("tenancy: id present in several tenants, using default", "kind", kind, "id", key)
		tenant = r.def
	default:
		// Store trouble: do not cache, do not drop the message.
		slog.Warn("tenancy: lookup failed, using default", "kind", kind, "id", key, "error", err)
		return r.def
	}
	if r.ttl > 0 {
		r.mu.Lock()
		cache[key] = entry{tenant: tenant, expiry: r.now().Add(r.ttl)}
		r.mu.Unlock()
	}
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
