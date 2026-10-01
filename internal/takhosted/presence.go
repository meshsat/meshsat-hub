package takhosted

import (
	"sync"
	"time"
)

// Which tenants have a client online (MESHSAT-1461).
//
// The connection to a tenant's TAK server used to be opened by the first
// position to forward and closed after fifteen minutes with nothing to send. That
// is right for a connection that only writes. It is wrong for one that also
// reads: a kit sitting indoors with no fix has nothing to send and still wants to
// see where its team is.
//
// So a tenant with a client online keeps its connections up. "Online" is read
// off the bus rather than out of the store: every bridge publishes its health
// every thirty seconds, and that is touched here by the bus handler in a few
// nanoseconds, with no query. A tenant nobody has heard from for presenceTTL is
// no longer present, and its connections go back to being closed when idle.

// presenceTTL is how long after its last health or birth a tenant still counts as
// having a client online: six missed reports.
const presenceTTL = 3 * time.Minute

type presence struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newPresence() *presence { return &presence{last: map[string]time.Time{}} }

// touch records that a client of this tenant was just heard, and reports whether
// the tenant was not present before -- which is when its connections want
// opening now rather than at the next tick.
func (p *presence) touch(tenantID string, now time.Time) (arrived bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	at, ok := p.last[tenantID]
	p.last[tenantID] = now
	return !ok || now.Sub(at) > presenceTTL
}

func (p *presence) present(tenantID string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	at, ok := p.last[tenantID]
	return ok && now.Sub(at) <= presenceTTL
}

// active returns the tenants present now, and forgets the ones that are not.
func (p *presence) active(now time.Time) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for t, at := range p.last {
		if now.Sub(at) > presenceTTL {
			delete(p.last, t)
			continue
		}
		out = append(out, t)
	}
	return out
}

func (p *presence) forget(tenantID string) {
	p.mu.Lock()
	delete(p.last, tenantID)
	p.mu.Unlock()
}
