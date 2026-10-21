package bridge

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/store"
	"github.com/meshsat/meshsat-hub/internal/store/dbwrap"
)

// egressRetryEvery is how often a birth the store has not taken is written
// again. A package variable so tests can shorten it.
var egressRetryEvery = 30 * time.Second

// pendingBirths holds every birth this subscriber has not written in full,
// per bridge: first the row, the declarations and the modem links, in one
// CreateOrUpdateBridge; then a record per declared modem (SetDeviceEgress).
//
// A later birth of the same bridge merges in: the row of the newest birth,
// per modem the newest declaration and the latest link. A birth that leaves a
// modem out therefore keeps what an earlier held birth said about it, and the
// first birth that names a modem keeps its link until that is written. The
// store orders every part again as it writes it (store.Bridge), so writing a
// held birth after a newer one landed, here or on another replica, changes
// nothing; a held birth is let go once the store has taken it, whether its
// parts were written or found older than what was stored.
//
// Until then the readers in this process take a held declaration first
// (EgressDeclarations.SetPending). A Hub that restarts before it writes a
// held birth reads the bridge's retained birth again on subscribe and holds
// that.
type pendingBirths struct {
	mu      sync.Mutex
	version uint64
	m       map[string]*pendingBirth // tenant + "/" + bridge
}

type pendingBirth struct {
	key, tenant string
	version     uint64 // changes with every birth merged in
	b           store.Bridge
	written     bool              // the row, declarations and links are stored
	records     map[string]string // IMEI to format still to record per modem
}

func newPendingBirths() *pendingBirths {
	return &pendingBirths{m: map[string]*pendingBirth{}}
}

// hold merges a birth into what the bridge has held and returns its key.
func (p *pendingBirths) hold(tenant string, nb *store.Bridge) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := tenant + "/" + nb.BridgeID
	p.version++
	h, ok := p.m[key]
	if !ok {
		h = &pendingBirth{key: key, tenant: tenant, b: *nb, records: map[string]string{}}
		h.b.Egress, h.b.Links = map[string]store.EgressDecl{}, map[string]int64{}
		p.m[key] = h
	} else if nb.BirthStamp >= h.b.BirthStamp {
		row := *nb
		row.Egress, row.Links = h.b.Egress, h.b.Links
		h.b = row
	}
	for imei, e := range nb.Egress {
		if old, ok := h.b.Egress[imei]; !ok || e.Stamp >= old.Stamp {
			h.b.Egress[imei] = e
			h.records[imei] = e.Format
		}
	}
	for imei, at := range nb.Links {
		if old, ok := h.b.Links[imei]; !ok || at >= old {
			h.b.Links[imei] = at
		}
	}
	h.written = false
	h.version = p.version
	return key
}

// snapshot is a copy of a held birth, safe to write from.
func (p *pendingBirths) snapshot(key string) (pendingBirth, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.m[key]
	if !ok {
		return pendingBirth{}, false
	}
	c := *h
	c.b.Egress, c.b.Links, c.records = maps.Clone(h.b.Egress), maps.Clone(h.b.Links), maps.Clone(h.records)
	return c, true
}

// written marks the row, declarations and links of a snapshot as stored; a
// birth merged in since keeps them pending.
func (p *pendingBirths) written(key string, version uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.m[key]; ok && h.version == version {
		h.written = true
		p.releaseLocked(h)
	}
}

func (p *pendingBirths) recorded(key, imei string, version uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.m[key]; ok && h.version == version {
		delete(h.records, imei)
		p.releaseLocked(h)
	}
}

func (p *pendingBirths) releaseLocked(h *pendingBirth) {
	if h.written && len(h.records) == 0 {
		delete(p.m, h.key)
	}
}

// drop lets a held birth go unwritten: the store refused it.
func (p *pendingBirths) drop(key string, version uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.m[key]; ok && h.version == version {
		delete(p.m, key)
	}
}

func (p *pendingBirths) keys() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.m))
	for k := range p.m {
		out = append(out, k)
	}
	return out
}

// declaration is the held declaration for a modem: of the held births that
// declare it, the one that names it latest.
func (p *pendingBirths) declaration(tenant, imei string) (bridgeID string, e store.EgressDecl, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var at int64
	for _, h := range p.m {
		d, has := h.b.Egress[imei]
		if h.tenant != tenant || !has {
			continue
		}
		if !ok || h.b.Links[imei] > at {
			bridgeID, e, ok, at = h.b.BridgeID, d, true, h.b.Links[imei]
		}
	}
	return bridgeID, e, ok
}

// PendingEgress is a declaration of the modem in a birth this subscriber has
// not written in full: the bridge that made it and the format.
func (s *Subscriber) PendingEgress(tenantID, imei string) (bridgeID, format string, ok bool) {
	bridgeID, e, ok := s.pending().declaration(tenantID, imei)
	return bridgeID, e.Format, ok
}

// PendingEgressEntry implements the readers' pending source (SetPending):
// PendingEgress with the stamp of the birth that declared it.
func (s *Subscriber) PendingEgressEntry(tenantID, imei string) (string, store.EgressDecl, bool) {
	return s.pending().declaration(tenantID, imei)
}

func (s *Subscriber) pending() *pendingBirths {
	s.pendingOnce.Do(func() { s.egressPending = newPendingBirths() })
	return s.egressPending
}

// ReconcileEgress writes every held birth again.
func (s *Subscriber) ReconcileEgress(ctx context.Context) {
	s.reconcileEgress(ctx, "")
}

// reconcileEgress is ReconcileEgress leaving out one held birth: a birth that
// has just been written tries the others at once, since the store has just
// taken a write, but not itself a second time.
func (s *Subscriber) reconcileEgress(ctx context.Context, skip string) {
	for _, key := range s.pending().keys() {
		if key != skip {
			_ = s.writeHeld(ctx, key)
		}
	}
}

// writeHeld writes a held birth: the row, declarations and links, then a
// record per declared modem. A birth the store refuses (ErrOwnedElsewhere) is
// let go; any other failure keeps it held. It returns the row's error.
func (s *Subscriber) writeHeld(ctx context.Context, key string) error {
	h, ok := s.pending().snapshot(key)
	if !ok {
		return nil
	}
	if !h.written {
		b := h.b
		// Written later than it came: a birth now older than staleThreshold
		// does not mark the bridge online, as a retained replay does not.
		if b.Online && b.BirthStamp > 0 && time.Since(time.Unix(0, b.BirthStamp)) > s.staleThreshold {
			b.Online = false
		}
		// Retry transient store errors (Galera 1213 deadlock, Postgres
		// 40P01/40001): simultaneous births from two Hub instances hit the
		// same rows.
		err := dbwrap.RetryShort(ctx, func() error {
			return s.store.CreateOrUpdateBridge(ctx, h.tenant, &b)
		})
		if errors.Is(err, store.ErrOwnedElsewhere) {
			s.pending().drop(key, h.version)
			return err
		}
		if err != nil {
			slog.Warn("bridge: birth not written yet; held and tried again", "error", err, "bridge", b.BridgeID)
			return err
		}
		s.pending().written(key, h.version)
	}
	for imei, format := range h.records {
		if err := s.store.SetDeviceEgress(ctx, h.tenant, imei, h.b.BridgeID, format); err != nil {
			slog.Warn("bridge: egress declaration not recorded for the modem yet; held and tried again",
				"error", err, "bridge", h.b.BridgeID, "imei", imei, "format", format)
			continue
		}
		s.pending().recorded(key, imei, h.version)
	}
	return nil
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
