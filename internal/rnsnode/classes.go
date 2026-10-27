package rnsnode

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/meshsat/meshsat-hub/internal/rns"
	"github.com/meshsat/meshsat-hub/internal/rnswire"
)

// Private tenants (MESHSAT-1722). A tenant whose Reticulum access mode is
// Private gets a node of its own: its own path, announce, link and dedup
// tables, the Hub's transport identity, and as interfaces only the
// connections on the public listener that presented that tenant's interface
// access code. Its addresses are registered there and nowhere else, so a
// stranger (or another tenant's member) can neither learn them from an
// announce or a path response nor reach them: a case nobody thought of fails
// closed, because the node simply has no other interface. The shared rns
// package is unchanged; the separation lives in which connections each
// node's transmitter can see.
//
// A public connection is classified by its first frame: one carrying an
// interface access code is tried against each Private tenant's key (and the
// platform's own HUB_RNS_PUBLIC_IFAC_*, which is the main node's), and a
// match binds the connection to that node for its lifetime; one without a
// code binds it to the main node, unless the platform's key is configured.
// Until then it is in no node's interface list. Classification happens
// before any node sees the frame, because a node silently drops a frame with
// an access code on an interface it holds no key for.

// ErrNotRunning is returned when the node is not running on this replica.
var ErrNotRunning = errors.New("rnsnode: the Reticulum node is not running on this replica")

const (
	// DefaultIFACTriesPerSecond bounds the key trials for unclassified
	// connections across the whole listener. Each trial is an HKDF and an
	// Ed25519 signature; the edge hides the client's address, so the bound
	// cannot be per source.
	DefaultIFACTriesPerSecond = 200
	// DefaultUnclassifiedTimeout closes a public connection that has not
	// been classified. A stock client sends a frame the moment it connects.
	DefaultUnclassifiedTimeout = 60 * time.Second
	// maxUnmatched closes a connection after this many frames with an access
	// code that matches no key.
	maxUnmatched = 3
)

type tenantNode struct {
	tenant string
	node   *rns.Node
	cancel context.CancelFunc

	mu         sync.RWMutex
	ifac       *rnswire.IFAC
	netname    string
	passphrase string
	conns      map[string]struct{}
}

func (t *tenantNode) key() *rnswire.IFAC {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.ifac
}

func (t *tenantNode) has(id string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.conns[id]
	return ok
}

func (t *tenantNode) ifaces() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, 0, len(t.conns))
	for id := range t.conns {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// binding is what one public connection has been classified as.
type binding struct {
	tn        *tenantNode // a Private tenant's node
	main      bool        // the main (Public) node
	unmatched int
	since     time.Time
}

// tenantTransmitter is a Private tenant's node's view of the world: the
// public connections bound to it, and nothing else.
type tenantTransmitter struct {
	s  *Service
	tn *tenantNode
}

func (t *tenantTransmitter) Transmit(ifaceID string, raw []byte) error {
	if !t.tn.has(ifaceID) {
		return ErrNoSuchInterface
	}
	p := t.s.pub.Load()
	if p == nil {
		return ErrNoSuchInterface
	}
	return p.Send(ifaceID, raw)
}

func (t *tenantTransmitter) Floodable() []string { return t.tn.ifaces() }

func (t *tenantTransmitter) HWMTU(string) int { return TCPHWMTU }

func (t *tenantTransmitter) Bitrate(string) int { return 0 }

// PrivateNode returns the tenant's own node, creating it on first use in
// this term. A changed network name or passphrase takes effect at once: the
// connections bound with the old key are closed and must present the new
// one. Called by lxmfhub when it registers a Private tenant's addresses.
func (s *Service) PrivateNode(tenantID, netname, passphrase string) (*rns.Node, error) {
	if netname == "" || passphrase == "" {
		return nil, errors.New("rnsnode: a private node needs a network name and a passphrase")
	}
	f, err := rnswire.NewIFAC(netname, passphrase, rnswire.DefaultIFACSize)
	if err != nil {
		return nil, err
	}
	s.pmu.Lock()
	if s.termCtx == nil {
		s.pmu.Unlock()
		return nil, ErrNotRunning
	}
	if tn := s.private[tenantID]; tn != nil {
		tn.mu.Lock()
		same := tn.netname == netname && tn.passphrase == passphrase
		var stale []string
		if !same {
			tn.ifac, tn.netname, tn.passphrase = f, netname, passphrase
			for id := range tn.conns {
				stale = append(stale, id)
			}
		}
		tn.mu.Unlock()
		s.pmu.Unlock()
		if len(stale) > 0 {
			slog.Info("rns: a private tenant's access code changed, closing its connections", "tenant", tenantID, "connections", len(stale))
			s.closePublic(stale)
		}
		return tn.node, nil
	}
	tn := &tenantNode{tenant: tenantID, ifac: f, netname: netname, passphrase: passphrase, conns: map[string]struct{}{}}
	node, err := rns.New(rns.Config{Identity: s.cfg.Identity, PathTTL: s.cfg.PathTTL, Now: s.cfg.Now}, &tenantTransmitter{s: s, tn: tn})
	if err != nil {
		s.pmu.Unlock()
		return nil, err
	}
	ctx, cancel := context.WithCancel(s.termCtx)
	tn.node, tn.cancel = node, cancel
	s.private[tenantID] = tn
	s.pmu.Unlock()
	node.Start(ctx)
	slog.Info("rns: private node started", "tenant", tenantID)
	return node, nil
}

// ReleasePrivate stops a tenant's own node and closes its connections: the
// tenant left the Private mode, or its address was disabled.
func (s *Service) ReleasePrivate(tenantID string) {
	s.pmu.Lock()
	tn := s.private[tenantID]
	delete(s.private, tenantID)
	s.pmu.Unlock()
	if tn == nil {
		return
	}
	tn.cancel()
	s.closePublic(tn.ifaces())
	slog.Info("rns: private node stopped", "tenant", tenantID)
}

// ForgetPaths drops the main node's cached paths to these destinations, from
// memory and from the store. An address that is not on the main node (its
// tenant chose Private or Off) must not be answered for from a cache: a
// transport-enabled client that heard it while it was Public announces it
// back, and the main node would hand that to anybody asking (MESHSAT-1722).
func (s *Service) ForgetPaths(hashes ...[rns.HashLen]byte) {
	n := s.main.Load()
	for _, h := range hashes {
		if n != nil {
			n.Paths().Remove(h)
		}
		if s.cfg.Store != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.cfg.Store.DeleteRNSPath(ctx, hex.EncodeToString(h[:]))
			cancel()
		}
	}
}

func (s *Service) closePublic(ids []string) {
	p := s.pub.Load()
	if p == nil {
		return
	}
	for _, id := range ids {
		p.Close(id)
	}
}

// onPublicChange tracks the public listener's connections: a new one is
// unclassified, a closed one is unbound from whichever node had it.
func (s *Service) onPublicChange(id string, up bool) {
	if up {
		s.bmu.Lock()
		s.bindings[id] = &binding{since: s.now()}
		s.bmu.Unlock()
		return
	}
	s.bmu.Lock()
	b := s.bindings[id]
	delete(s.bindings, id)
	s.bmu.Unlock()
	if b == nil {
		return
	}
	switch {
	case b.tn != nil:
		b.tn.mu.Lock()
		delete(b.tn.conns, id)
		b.tn.mu.Unlock()
		b.tn.node.SetIFAC(id, nil)
	case b.main && s.cfg.PublicIFAC != nil:
		if n := s.main.Load(); n != nil {
			n.SetIFAC(id, nil)
		}
	}
}

func (s *Service) bindingOf(id string) (tn *tenantNode, main, known bool) {
	s.bmu.RLock()
	defer s.bmu.RUnlock()
	b := s.bindings[id]
	if b == nil {
		return nil, false, false
	}
	return b.tn, b.main, true
}

// mainBound lists the public connections bound to the main node.
func (s *Service) mainBound() []string {
	s.bmu.RLock()
	defer s.bmu.RUnlock()
	var out []string
	for id, b := range s.bindings {
		if b.main {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Service) isMainBound(id string) bool {
	_, main, _ := s.bindingOf(id)
	return main
}

// onPublicFrame classifies and delivers one frame from the public listener.
func (s *Service) onPublicFrame(id string, raw []byte) {
	tn, main, known := s.bindingOf(id)
	if !known {
		return // closed meanwhile
	}
	switch {
	case tn != nil:
		if !tn.node.Inbound(append([]byte(nil), raw...), id) {
			s.publicDropped.Add(1)
		}
		return
	case main:
		s.deliverMain(id, raw)
		return
	}
	if !rnswire.HasIFACFlag(raw) {
		if s.cfg.PublicIFAC != nil {
			// The platform requires its own code on the main node.
			s.unmatched(id)
			return
		}
		s.bindMain(id)
		s.deliverMain(id, raw)
		return
	}
	if f := s.cfg.PublicIFAC; f != nil {
		if !s.tries.allow(s.now(), 1) {
			s.ifacThrottled.Add(1)
			return
		}
		if _, err := f.Unwrap(raw); err == nil {
			s.bindMain(id)
			s.deliverMain(id, raw)
			return
		}
	}
	s.pmu.RLock()
	cands := make([]*tenantNode, 0, len(s.private))
	for _, c := range s.private {
		cands = append(cands, c)
	}
	s.pmu.RUnlock()
	for _, c := range cands {
		f := c.key()
		if f == nil {
			continue
		}
		// One token per key tried, so the budget bounds the work and many
		// Private tenants slow classification rather than block it.
		if !s.tries.allow(s.now(), 1) {
			s.ifacThrottled.Add(1)
			return
		}
		if _, err := f.Unwrap(raw); err != nil {
			continue
		}
		if !s.bindTenant(id, c, f) {
			return
		}
		if !c.node.Inbound(append([]byte(nil), raw...), id) {
			s.publicDropped.Add(1)
		}
		return
	}
	s.unmatched(id)
}

func (s *Service) deliverMain(id string, raw []byte) {
	s.mu.RLock()
	n := s.node
	s.mu.RUnlock()
	// The node rewrites the hop byte in place, so it works on a copy.
	if n != nil && n.Inbound(append([]byte(nil), raw...), id) {
		return
	}
	// Nothing from the public listener reaches the legacy layer
	// (MESHSAT-1720): see onTCPFrame.
	s.publicDropped.Add(1)
}

func (s *Service) bindMain(id string) {
	s.bmu.Lock()
	b := s.bindings[id]
	if b != nil {
		b.main = true
	}
	s.bmu.Unlock()
	if b == nil {
		return
	}
	if f := s.cfg.PublicIFAC; f != nil {
		if n := s.main.Load(); n != nil {
			n.SetIFAC(id, f)
			// Closed meanwhile: its close found no key to clear.
			if !s.stillBound(id, b) {
				n.SetIFAC(id, nil)
			}
		}
	}
}

// stillBound reports whether a connection's binding is the one a caller set
// up: false when the connection closed while the caller was busy with the
// node, and the caller must undo what it did.
func (s *Service) stillBound(id string, b *binding) bool {
	s.bmu.RLock()
	defer s.bmu.RUnlock()
	return s.bindings[id] == b
}

// bindTenant binds a connection to a Private tenant's node; false when the
// connection or the node went away meanwhile.
func (s *Service) bindTenant(id string, tn *tenantNode, f *rnswire.IFAC) bool {
	s.pmu.RLock()
	live := s.private[tn.tenant] == tn
	s.pmu.RUnlock()
	if !live {
		return false
	}
	s.bmu.Lock()
	b := s.bindings[id]
	if b != nil {
		b.tn = tn
	}
	s.bmu.Unlock()
	if b == nil {
		return false
	}
	tn.mu.Lock()
	tn.conns[id] = struct{}{}
	tn.mu.Unlock()
	tn.node.SetIFAC(id, f)
	// The connection may have closed while the key and the list were being
	// written: its close then found nothing to remove, so remove it here.
	if !s.stillBound(id, b) {
		tn.mu.Lock()
		delete(tn.conns, id)
		tn.mu.Unlock()
		tn.node.SetIFAC(id, nil)
		return false
	}
	slog.Info("rns: public connection bound to a private tenant", "iface", id, "tenant", tn.tenant)
	return true
}

func (s *Service) unmatched(id string) {
	s.ifacUnmatched.Add(1)
	s.bmu.Lock()
	b := s.bindings[id]
	n := 0
	if b != nil {
		b.unmatched++
		n = b.unmatched
	}
	s.bmu.Unlock()
	if n >= maxUnmatched {
		slog.Info("rns: closing a public connection whose access code matches no network", "iface", id)
		s.closePublic([]string{id})
	}
}

// sweepUnclassified closes public connections that never sent a frame
// that classified them.
func (s *Service) sweepUnclassified(ctx context.Context) {
	timeout := s.cfg.UnclassifiedTimeout
	if timeout <= 0 {
		timeout = DefaultUnclassifiedTimeout
	}
	t := time.NewTicker(timeout / 4)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cutoff := s.now().Add(-timeout)
		var stale []string
		s.bmu.RLock()
		for id, b := range s.bindings {
			if b.tn == nil && !b.main && b.since.Before(cutoff) {
				stale = append(stale, id)
			}
		}
		s.bmu.RUnlock()
		if len(stale) > 0 {
			s.closePublic(stale)
		}
	}
}

// tryBudget is a token bucket over key trials.
type tryBudget struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	last   time.Time
}

func newTryBudget(perSecond int) *tryBudget {
	if perSecond <= 0 {
		perSecond = DefaultIFACTriesPerSecond
	}
	return &tryBudget{rate: float64(perSecond), tokens: float64(perSecond)}
}

func (b *tryBudget) allow(now time.Time, n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.last.IsZero() {
		b.tokens += now.Sub(b.last).Seconds() * b.rate
		if b.tokens > b.rate {
			b.tokens = b.rate
		}
	}
	b.last = now
	if b.tokens < float64(n) {
		return false
	}
	b.tokens -= float64(n)
	return true
}
