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
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/meshsat/meshsat-hub/internal/store"
)

// ownerRecheckEvery bounds how often a refusal re-reads an owner from the store,
// so somebody publishing forgeries as fast as the broker allows costs one
// primary-key read per id per second rather than one per message per consumer.
const ownerRecheckEvery = time.Second

// topicRefused counts messages dropped because the namespace they were published
// in belongs to somebody other than the device's registered owner (MESHSAT-1469).
// One message counts once per consumer on each replica. Anything above zero is
// either a tenant naming a modem that is not theirs or a modem registered to the
// wrong account, and both want a person to look.
var topicRefused = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "meshsat_hub_tenancy_topic_refused_total",
	Help: "Messages refused because their topic's tenant is not the registered owner of the device or bridge they name.",
}, []string{"kind"})

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
	read   time.Time // when the store last answered for this id
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

// ForDeviceTopic answers which tenant a satellite provider's delivery belongs to.
// The store is authoritative for registered devices; an unregistered device is
// placed in topicTenant when that tenant exists, else the default.
//
// It is for callers that have NO tenant of their own to believe: a provider
// delivers by IMEI, on a path that may be the shared platform account. Do not
// use it on a message read off the bus, where the namespace is what the broker
// authenticated and the id is whatever the publisher typed: that is
// AdmitDeviceTopic.
func (r *Resolver) ForDeviceTopic(ctx context.Context, imei, topicTenant string) string {
	owner, known := r.known(ctx, r.devices, imei, r.store.LookupDeviceTenant, "device")
	return r.reconcile(ctx, owner, known, topicTenant, "device", imei)
}

// ForBridgeTopic is ForDeviceTopic for bridge topics.
func (r *Resolver) ForBridgeTopic(ctx context.Context, bridgeID, topicTenant string) string {
	owner, known := r.known(ctx, r.bridges, bridgeID, r.store.LookupBridgeTenant, "bridge")
	return r.reconcile(ctx, owner, known, topicTenant, "bridge", bridgeID)
}

// AdmitDeviceTopic answers which tenant a message read off the bus belongs to,
// or ok=false when it must be dropped (MESHSAT-1469).
//
// Every bridge may publish mo/decoded, position and sos for ANY device id inside
// its own tenant's namespace (bridge.NATSPermissions), so the id proves nothing
// and the namespace is the only thing the broker vouched for. A registered
// device named in a customer namespace that is not its owner's is therefore a
// forgery as far as the Hub can tell, and it goes nowhere: not to the owner,
// whose routes, SOS chain and history it would write into, and not to the
// publisher either.
//
// The PLATFORM namespace is the exception, deliberately. Only the Hub and the
// operator's own bridges can publish there, so it crosses no tenant boundary,
// and it is where a provider handler publishes when it could not read the owner.
// Refusing that would lose a real SOS during a database blip; it keeps resolving
// to the owner of record.
//
// A failed lookup with no owner on record admits: the store being down must not
// cost a real message. An owner this replica already knows stands when the store
// cannot confirm it. Admitting there would open the check to every forgery
// during any database blip, to spare a real message only if a modem also changed
// hands inside the cache's TTL at that moment.
func (r *Resolver) AdmitDeviceTopic(ctx context.Context, imei, topicTenant string) (string, bool) {
	// A phone number is a sender, never a device (MESHSAT-1688). The API has
	// refused to register one since store.ValidDeviceID, but a row from before
	// that must not take the number's traffic out of the tenant it arrived at,
	// nor, in a customer namespace, get it refused.
	if strings.HasPrefix(imei, "+") {
		return r.reconcile(ctx, r.def, false, topicTenant, "device", imei), true
	}
	return r.admit(ctx, r.devices, imei, r.store.LookupDeviceTenant, topicTenant, "device")
}

// AdmitBridgeTopic is AdmitDeviceTopic for a bridge id a message names.
func (r *Resolver) AdmitBridgeTopic(ctx context.Context, bridgeID, topicTenant string) (string, bool) {
	return r.admit(ctx, r.bridges, bridgeID, r.store.LookupBridgeTenant, topicTenant, "bridge")
}

// RefreshDevice re-reads a device's owner into the cache, for a satellite
// provider's delivery to do before it picks the namespace to publish in. The
// consumers refuse a namespace that is not the owner's, so a stale answer here,
// in the seconds after a modem changes hands, would lose a real message. A
// provider delivers rarely enough that one primary-key read each is nothing. A
// failed read keeps what the cache had.
func (r *Resolver) RefreshDevice(ctx context.Context, imei string) {
	if imei == "" || r.store == nil || r.ttl <= 0 {
		return
	}
	r.fetch(ctx, r.devices, imei, r.store.LookupDeviceTenant, "device")
}

func (r *Resolver) admit(ctx context.Context, cache map[string]entry, key string, lookup func(context.Context, string) (string, error), topicTenant, kind string) (string, bool) {
	owner, known := r.known(ctx, cache, key, lookup, kind)
	if known && r.isCustomerNamespace(topicTenant) && topicTenant != owner {
		// Before refusing, make sure this replica is not simply behind: the
		// device may have changed hands inside the cache's TTL.
		owner, known = r.reread(ctx, cache, key, lookup, kind, owner, known)
		if known && topicTenant != owner {
			topicRefused.WithLabelValues(kind).Inc()
			slog.Warn("tenancy: message refused, its topic names a tenant that does not own the id", "kind", kind, "id", key, "topic_tenant", topicTenant, "owner", owner)
			return "", false
		}
	}
	return r.reconcile(ctx, owner, known, topicTenant, kind, key), true
}

func (r *Resolver) isCustomerNamespace(topicTenant string) bool {
	return topicTenant != "" && topicTenant != r.def
}

// reread asks the store again unless it answered for this id moments ago, and
// keeps the answer it was given when the store cannot say. The bound is what
// keeps forgeries from becoming a database load; its price is that a modem moved
// to another tenant can be refused for at most ownerRecheckEvery on a replica
// that read the old owner just before the move.
func (r *Resolver) reread(ctx context.Context, cache map[string]entry, key string, lookup func(context.Context, string) (string, error), kind, owner string, known bool) (string, bool) {
	if r.ttl <= 0 {
		return owner, known // known() has only just read it
	}
	r.mu.Lock()
	e, ok := cache[key]
	r.mu.Unlock()
	if ok && r.now().Sub(e.read) < ownerRecheckEvery {
		return owner, known
	}
	if tenant, found, definite := r.fetch(ctx, cache, key, lookup, kind); definite {
		return tenant, found
	}
	return owner, known
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
	tenant, found, _ := r.fetch(ctx, cache, key, lookup, kind)
	return tenant, found
}

// fetch asks the store and caches the answer. definite is false when the store
// could not say, in which case nothing is cached and the default comes back.
func (r *Resolver) fetch(ctx context.Context, cache map[string]entry, key string, lookup func(context.Context, string) (string, error), kind string) (tenant string, found, definite bool) {
	tenant, err := lookup(ctx, key)
	found = err == nil && tenant != ""
	switch {
	case found:
	case errors.Is(err, store.ErrNotFound):
		tenant = r.def
	case errors.Is(err, store.ErrAmbiguousTenant):
		slog.Warn("tenancy: id present in several tenants, using default", "kind", kind, "id", key)
		tenant = r.def
	default:
		slog.Warn("tenancy: lookup failed, using default", "kind", kind, "id", key, "error", err)
		return r.def, false, false
	}
	if r.ttl > 0 {
		now := r.now()
		r.mu.Lock()
		cache[key] = entry{tenant: tenant, known: found, expiry: now.Add(r.ttl), read: now}
		r.mu.Unlock()
	}
	return tenant, found, true
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

// ForgetTenant drops every cached answer that points at this tenant, for a
// purge or a closure (MESHSAT-1109).
//
// It scans rather than taking a list of ids, because the caller does not have
// one: by the time a purge announces itself the device and bridge rows are
// already deleted, so there is nothing left to enumerate. The maps are small --
// one entry per device or bridge that has sent traffic within the TTL -- and
// this runs once per purged tenant, not per message.
//
// The tenant's own existence entry goes too. Leaving it would keep Known()
// answering "yes, that tenant exists" for up to the TTL after its rows were
// destroyed, which is exactly the window in which an unregistered device naming
// it in a topic would be adopted into a tenant that is gone.
func (r *Resolver) ForgetTenant(tenantID string) {
	if r == nil || tenantID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range []map[string]entry{r.devices, r.bridges} {
		for k, e := range m {
			if e.tenant == tenantID {
				delete(m, k)
			}
		}
	}
	delete(r.tenants, tenantID)
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
