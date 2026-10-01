package takhosted

import (
	"sync"
	"time"
)

// Rate limits on exported CoT (MESHSAT-1458).
//
// A kit exports its own position twice a minute and the mesh traffic it relays
// for other nodes; an app exports its position on each fix. These budgets are
// well above that and well below what would matter to the Hub, so a client that
// is working never meets them and one that is not cannot use the Hub to flood a
// TAK server, nor crowd other tenants out of the forwarder's queue.
//
// Per sender AND per tenant: the sender's budget stops one client; the tenant's
// stops a tenant with many clients, or one client publishing under more than one
// name, from being a larger problem than its plan's worth of kits.
//
// The emergency budget is separate and is the point of the split. An SOS must
// not be dropped because the same kit, or another kit of the same tenant, has
// used its budget on position updates. It is small, so labelling everything an
// emergency buys a sender less than not doing so.
//
// A token bucket from the standard library's arithmetic rather than
// golang.org/x/time/rate: that module is only an indirect dependency here, and
// eight lines do not justify promoting it to a direct one.
const (
	senderRate, senderBurst             = 10.0, 50.0
	tenantRate, tenantBurst             = 50.0, 200.0
	senderUrgentRate, senderUrgentBurst = 2.0, 10.0
	tenantUrgentRate, tenantUrgentBurst = 10.0, 40.0

	// limiterIdle is how long a bucket nobody has drawn on is kept.
	limiterIdle = 10 * time.Minute
)

type bucket struct {
	tokens float64
	last   time.Time
}

// limiter is a set of token buckets by key. Safe for concurrent use; take never
// blocks, which is what lets the bus handler call it.
type limiter struct {
	mu sync.Mutex
	m  map[string]*bucket
}

func newLimiter() *limiter { return &limiter{m: map[string]*bucket{}} }

// take draws one token from key's bucket, which refills at rate per second up to
// burst. A new key starts full.
func (l *limiter) take(key string, rate, burst float64, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.m[key]
	if !ok {
		b = &bucket{tokens: burst, last: now}
		l.m[key] = b
	}
	if dt := now.Sub(b.last).Seconds(); dt > 0 {
		b.tokens += dt * rate
		if b.tokens > burst {
			b.tokens = burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// prune drops the buckets nobody has drawn on for limiterIdle. A key is one
// bridge's topic or one tenant, so the map is bounded by what is registered; this
// keeps it to what is active.
func (l *limiter) prune(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.m {
		if now.Sub(b.last) > limiterIdle {
			delete(l.m, k)
		}
	}
}

// forget drops every bucket whose key starts with prefix.
func (l *limiter) forget(prefix string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k := range l.m {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(l.m, k)
		}
	}
}
