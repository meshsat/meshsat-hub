package bridge

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// egressRetryEvery is how often a declaration whose write failed is tried
// again. A package variable so tests can shorten it.
var egressRetryEvery = 30 * time.Second

// egressPending holds the declarations a birth made until both of their
// writes have succeeded: the bridge's merged map (in the statement that saves
// the birth, or MergeBridgeEgress on a retry) and the per-modem record. Until
// then it is the newest word on the modem, and the readers in this process
// take it over the database (EgressDeclarations.SetPending). A later
// declaration for the same modem replaces it; a birth without the modem's
// IMEI does not touch it. A Hub that restarts before the retry succeeds reads
// the bridge's retained birth again on subscribe, which holds and writes it
// anew.
type egressPending struct {
	mu  sync.Mutex
	seq uint64
	m   map[string]*pendingEgress // tenant + "/" + imei
}

type pendingEgress struct {
	tenant, imei, bridge, format string
	seq                          uint64
	mapDone, recordDone          bool
}

func newEgressPending() *egressPending {
	return &egressPending{m: map[string]*pendingEgress{}}
}

// hold records the declarations of one birth as pending and returns their
// sequence numbers, which done uses so that a newer hold is never released by
// an older write.
func (p *egressPending) hold(tenant, bridge string, declared map[string]string) map[string]uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	seqs := make(map[string]uint64, len(declared))
	for imei, format := range declared {
		p.seq++
		p.m[tenant+"/"+imei] = &pendingEgress{tenant: tenant, imei: imei, bridge: bridge, format: format, seq: p.seq}
		seqs[imei] = p.seq
	}
	return seqs
}

// done marks one write of a held declaration as made, and lets it go when
// both are.
func (p *egressPending) done(tenant, imei string, seq uint64, mapWrite bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.m[tenant+"/"+imei]
	if !ok || e.seq != seq {
		return
	}
	if mapWrite {
		e.mapDone = true
	} else {
		e.recordDone = true
	}
	if e.mapDone && e.recordDone {
		delete(p.m, tenant+"/"+imei)
	}
}

// drop lets a held declaration go unwritten: its birth was refused.
func (p *egressPending) drop(tenant, imei string, seq uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.m[tenant+"/"+imei]; ok && e.seq == seq {
		delete(p.m, tenant+"/"+imei)
	}
}

// PendingEgress is a declaration of the modem not yet written in full: the
// bridge that made it and the format.
func (p *egressPending) PendingEgress(tenant, imei string) (bridge, format string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.m[tenant+"/"+imei]
	if !ok {
		return "", "", false
	}
	return e.bridge, e.format, true
}

func (p *egressPending) snapshot() []pendingEgress {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]pendingEgress, 0, len(p.m))
	for _, e := range p.m {
		out = append(out, *e)
	}
	return out
}

// PendingEgress implements the readers' pending source (SetPending): a
// declaration this subscriber took from a birth but has not written in full.
func (s *Subscriber) PendingEgress(tenantID, imei string) (bridgeID, format string, ok bool) {
	return s.pending().PendingEgress(tenantID, imei)
}

func (s *Subscriber) pending() *egressPending {
	s.pendingOnce.Do(func() { s.egressPending = newEgressPending() })
	return s.egressPending
}

// ReconcileEgress writes again every held declaration whose write failed:
// the bridge's map with MergeBridgeEgress, the modem's record with
// SetDeviceEgress. What succeeds is let go; the rest waits for the next try.
func (s *Subscriber) ReconcileEgress(ctx context.Context) {
	s.reconcileEgress(ctx, nil)
}

// reconcileEgress is ReconcileEgress leaving out the declarations in skip
// (by sequence): a birth that has just written tries the older ones at once,
// since the store has just taken a write, but not its own a second time.
func (s *Subscriber) reconcileEgress(ctx context.Context, skip map[string]uint64) {
	for _, e := range s.pending().snapshot() {
		if seq, ok := skip[e.imei]; ok && seq == e.seq {
			continue
		}
		if !e.mapDone {
			if err := s.store.MergeBridgeEgress(ctx, e.tenant, e.bridge, map[string]string{e.imei: e.format}); err != nil {
				slog.Warn("bridge: egress declaration still not written to the bridge", "error", err, "bridge", e.bridge, "imei", e.imei, "format", e.format)
			} else {
				s.pending().done(e.tenant, e.imei, e.seq, true)
			}
		}
		if !e.recordDone {
			if err := s.store.SetDeviceEgress(ctx, e.tenant, e.imei, e.bridge, e.format); err != nil {
				slog.Warn("bridge: egress declaration still not recorded for the modem", "error", err, "bridge", e.bridge, "imei", e.imei, "format", e.format)
			} else {
				s.pending().done(e.tenant, e.imei, e.seq, false)
			}
		}
	}
}

// reconcileEgressLoop runs ReconcileEgress every egressRetryEvery until stop
// closes.
func (s *Subscriber) reconcileEgressLoop(stop <-chan struct{}) {
	t := time.NewTicker(egressRetryEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			s.ReconcileEgress(ctx)
			cancel()
		}
	}
}
