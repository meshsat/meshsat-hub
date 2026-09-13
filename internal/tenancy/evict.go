package tenancy

import (
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/meshsat/meshsat-hub/internal/bus"
)

// PurgeTopic carries "this tenant is gone, drop everything you hold for it"
// between replicas.
//
// # Why this exists (MESHSAT-1109)
//
// Five in-memory caches each documented a Forget method as "what the purge path
// calls", and `git grep "\.Forget(" | grep -v _test.go` returned NOTHING. The
// purge job invalidated none of them, so a tenant whose account had been erased
// went on living in every replica's memory.
//
// For some of them that was untidy and bounded: tenancy.Resolver expires in 30 s
// and StatusCache in 15 s. For takhosted.IdentityKeeper it was not bounded at
// all -- it holds a tenant's TAK identity until the certificate's own renewal
// window, which is measured in weeks. A purged tenant kept a usable hosted-TAK
// identity in memory on a platform that now takes money.
//
// # Why it is announced rather than just called
//
// The purge job is a leader singleton. A Forget called there evicts the leader
// and leaves every other replica holding the tenant, which is the same bug with
// a smaller blast radius and a much better disguise -- it would test green on a
// single-replica dev box and fail in production two thirds of the time.
//
// So this follows StatusTopic exactly: evict here, say so on the bus, and every
// replica drops it too.
const PurgeTopic = "meshsat/hub/tenant/purged"

type purgeEvent struct {
	TenantID string `json:"tenant_id"`
}

// TenantForgetter is one in-memory cache that can drop everything it holds for
// a tenant.
//
// It is deliberately a one-method interface declared HERE rather than an import
// of internal/takhosted, for the reason spelled out on PurgeJob.TAKInstanceDeleter:
// eight packages that carry field traffic import this one, so an import of
// takhosted would drag the Kubernetes custom-resource client and its in-cluster
// REST config onto every ingest path in the Hub.
type TenantForgetter interface {
	ForgetTenant(tenantID string)
}

// Evictor drops a tenant from every cache that holds one, on every replica.
type Evictor struct {
	bus bus.MessageBus
	log *slog.Logger

	mu     sync.RWMutex
	caches []TenantForgetter
}

// NewEvictor returns an Evictor. A nil bus is allowed: eviction then happens
// only on the replica that calls Evict, which is what a single-replica or
// broker-less deployment gets.
func NewEvictor(b bus.MessageBus, log *slog.Logger) *Evictor {
	if log == nil {
		log = slog.Default()
	}
	return &Evictor{bus: b, log: log}
}

// Register adds a cache. Called at startup, once per cache.
//
// A nil interface value is ignored rather than registered, because the caches
// are optional: a Hub built without hosted TAK has no IdentityKeeper, and a
// typed nil pointer stored in an interface is not == nil, so this checks the
// concrete value too by leaving that to each ForgetTenant -- all five return
// early on a nil receiver.
func (e *Evictor) Register(c TenantForgetter) {
	if e == nil || c == nil {
		return
	}
	e.mu.Lock()
	e.caches = append(e.caches, c)
	e.mu.Unlock()
}

// Subscribe listens for evictions performed by another replica. Safe to call
// with no bus.
func (e *Evictor) Subscribe() error {
	if e == nil || e.bus == nil {
		return nil
	}
	return e.bus.Subscribe(PurgeTopic, 1, func(_ string, payload []byte) {
		var ev purgeEvent
		if err := json.Unmarshal(payload, &ev); err != nil || ev.TenantID == "" {
			return
		}
		e.evictLocal(ev.TenantID)
	})
}

// Evict drops the tenant from every registered cache here, and tells the other
// replicas to do the same.
func (e *Evictor) Evict(tenantID string) {
	if e == nil || tenantID == "" {
		return
	}
	e.evictLocal(tenantID)
	if e.bus == nil {
		return
	}
	if err := e.bus.PublishJSON(PurgeTopic, 1, false, purgeEvent{TenantID: tenantID}); err != nil {
		// Not fatal and not worth failing a purge over: the rows are already
		// gone, so nothing NEW can resolve to this tenant anywhere. What
		// survives is stale cached answers on the other replicas, bounded by
		// their own TTLs -- except the identity keeper, which is why this is a
		// Warn naming the tenant rather than a debug line.
		e.log.Warn("purge: other replicas were not told to forget this tenant; "+
			"their cached answers for it survive until their own TTLs expire",
			"tenant", tenantID, "error", err)
	}
}

func (e *Evictor) evictLocal(tenantID string) {
	e.mu.RLock()
	caches := make([]TenantForgetter, len(e.caches))
	copy(caches, e.caches)
	e.mu.RUnlock()
	for _, c := range caches {
		c.ForgetTenant(tenantID)
	}
}
