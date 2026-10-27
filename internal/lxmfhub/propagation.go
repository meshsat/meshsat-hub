package lxmfhub

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/meshsat/meshsat-hub/internal/lxmf"
	"github.com/meshsat/meshsat-hub/internal/msgpack"
	"github.com/meshsat/meshsat-hub/internal/rns"
	"github.com/meshsat/meshsat-hub/internal/rnswire"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// A tenant's LXMF propagation node (MESHSAT-1555 P5): the lxmf.propagation
// destination on the tenant's identity. A client hands it a message for a
// peer that is offline (one per transfer, with a propagation stamp); the node
// holds the encrypted bytes it cannot read and serves them to the recipient's
// client on a /get request after it identified. A message for one of this
// Hub's own tenant addresses is decrypted and filed into that inbox at once.
// Peering between propagation nodes is not offered: /offer answers no access.

const (
	defaultPropagationMaxBytes = 50 << 20
	pnSweepEvery               = 10 * time.Minute
	// pnProtocolVersion is the LXMF release whose propagation protocol this
	// implements, announced as the node's version.
	pnProtocolVersion = "1.2.0"
)

func (s *Service) registerPNLocked(tr *tenantRouter) {
	tid := tr.id.TenantID
	// The announce closure reads what it needs from the registration, never
	// the service mutex: the node calls it under its own lock while register
	// holds ours and calls into the node, the other way round.
	cost, name := tr.id.PropagationStampCost, tr.id.DisplayName
	d := tr.node.AddDestination(&rns.Destination{
		Name: lxmf.PropagationName, Identity: tr.id.ident, AcceptLinks: true, ProveAll: true,
		AppData:      func() []byte { return s.pnAnnounce(cost, name).AppData() },
		OnLinkPacket: func(l *rns.Link, plain []byte, pkt *rns.Packet) { s.pnInbound(tid, l, plain) },
		AcceptResources: func(size int) bool {
			return size > 0 && size <= lxmf.DefaultSyncLimitKB*1000
		},
		OnLinkResource: func(l *rns.Link, data []byte) { s.pnInbound(tid, l, data) },
	})
	d.RegisterRequestHandler(lxmf.GetPath, func(req *rns.Request) (msgpack.Value, bool) { return s.pnGet(tid, req), true })
	d.RegisterRequestHandler(lxmf.OfferPath, func(*rns.Request) (msgpack.Value, bool) { return lxmf.Int(lxmf.ErrorNoAccess), true })
	tr.pn = d
}

func (s *Service) pnAnnounce(cost int, name string) lxmf.PropagationAnnounce {
	return lxmf.PropagationAnnounce{Timebase: s.now().Unix(), Enabled: true, TransferLimitKB: lxmf.DefaultTransferLimitKB,
		SyncLimitKB: lxmf.DefaultSyncLimitKB, StampCost: cost, StampCostFlex: lxmf.DefaultPropagationCostFlex,
		PeeringCost: lxmf.DefaultPeeringCost, Name: name, Version: pnProtocolVersion}
}

// pnInbound is a client's transfer to the propagation node: one message with
// its propagation stamp, as a link packet or a resource.
func (s *Service) pnInbound(tenantID string, l *rns.Link, data []byte) {
	s.mu.Lock()
	tr := s.routers[tenantID]
	s.mu.Unlock()
	if tr == nil {
		return
	}
	node := tr.node
	_, msgs, err := lxmf.UnpackPropagation(data)
	if err != nil {
		slog.Debug("lxmf: propagation transfer unreadable", "tenant", tenantID, "error", err)
		return
	}
	if len(msgs) != 1 {
		// Several messages per transfer is what peered propagation nodes do
		// after presenting a peering key; a client delivers one.
		slog.Info("lxmf: propagation transfer with several messages from an unpeered link, closing it", "tenant", tenantID, "messages", len(msgs))
		node.Links().Teardown(l)
		return
	}
	minCost := tr.id.PropagationStampCost - lxmf.DefaultPropagationCostFlex
	if minCost < 0 {
		minCost = 0
	}
	held, ok := lxmf.ValidatePNStamp(msgs[0], minCost)
	if !ok {
		slog.Info("lxmf: propagation transfer with an invalid stamp, closing the link", "tenant", tenantID, "min_cost", minCost)
		if ct, err := l.Encrypt(lxmf.Pack(lxmf.Array(lxmf.Int(lxmf.ErrorInvalidStamp)))); err == nil {
			node.Links().SendContext(l, 0x00, 0x00, ct)
		}
		node.Links().Teardown(l)
		return
	}
	source := ""
	if pub := l.RemoteIdentity(); pub != nil {
		if id, err := rnswire.IdentityFromPublicBytes(pub); err == nil {
			h := id.IdentityHash()
			source = hex.EncodeToString(h[:])
		}
	}
	// For an address this Hub holds, the node is the recipient's own router:
	// decrypt and file, as upstream delivers to its own delivery destinations.
	if owner := s.routerForAddress(node, held.Destination); owner != nil {
		plain, _, err := owner.id.ident.Decrypt(held.Data[lxmf.DestLen:], owner.router.Dest.Ratchets())
		if err != nil {
			slog.Debug("lxmf: propagated message for a local address did not decrypt", "tenant", owner.id.TenantID, "error", err)
			return
		}
		packed := append(append([]byte(nil), held.Destination[:]...), plain...)
		if _, err := owner.router.Inbound(packed, lxmf.Propagated, l.Iface); err != nil {
			slog.Debug("lxmf: propagated message for a local address refused", "tenant", owner.id.TenantID, "error", err)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	row := &store.LXMFHeldMessage{ID: hex.EncodeToString(held.TransientID[:]), Destination: hex.EncodeToString(held.Destination[:]),
		Data: held.Data, Size: len(held.Data), StampValue: held.StampValue, Source: source, ReceivedAt: s.now()}
	maxBytes := s.cfg.PropagationMaxBytes
	if used, err := s.store.LXMFHeldBytes(ctx, tenantID); err == nil && used+int64(len(held.Data)) > maxBytes {
		if _, err := s.store.PruneLXMFHeld(ctx, tenantID, s.now().Add(-lxmf.MessageExpiry), maxBytes-int64(len(held.Data))); err != nil {
			slog.Warn("lxmf: propagation store prune failed", "tenant", tenantID, "error", err)
		}
	}
	if err := s.store.InsertLXMFHeld(ctx, tenantID, row); err != nil && !errors.Is(err, store.ErrDuplicate) {
		slog.Error("lxmf: propagation store write failed", "tenant", tenantID, "error", err)
		return
	}
	s.heldIn.Add(1)
	slog.Info("lxmf: holding a propagated message", "tenant", tenantID, "transient_id", row.ID[:12], "for", row.Destination, "bytes", row.Size, "stamp_value", held.StampValue)
}

// routerForAddress finds the tenant whose delivery address a message is for,
// among the addresses on the same node only: a propagation node never files
// a message across into another network (MESHSAT-1722), it holds it.
func (s *Service) routerForAddress(node *rns.Node, dest [lxmf.DestLen]byte) *tenantRouter {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, tr := range s.routers {
		if tr.node == node && tr.router.Hash() == dest {
			return tr
		}
	}
	return nil
}

// pnGet serves LXMPeer.MESSAGE_GET_PATH: [None, None] lists what is held for
// the identified client's delivery address, smallest first; [wants, haves,
// limit_kB] purges the haves and returns the wanted messages within the
// client's transfer limit, stamps stripped.
func (s *Service) pnGet(tenantID string, req *rns.Request) msgpack.Value {
	if req.RemoteIdentity == nil {
		return lxmf.Int(lxmf.ErrorNoIdentity)
	}
	deliveryHash := req.RemoteIdentity.DestHash(lxmf.DeliveryName)
	destHex := hex.EncodeToString(deliveryHash[:])
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	d := req.Data
	if d.Kind != msgpack.KindArray || len(d.Array) < 2 {
		return lxmf.Int(lxmf.ErrorInvalidData)
	}
	want, have := d.Array[0], d.Array[1]
	if want.Kind == msgpack.KindNil && have.Kind == msgpack.KindNil {
		list, err := s.store.ListLXMFHeldFor(ctx, tenantID, destHex)
		if err != nil {
			slog.Warn("lxmf: propagation list failed", "tenant", tenantID, "error", err)
			return lxmf.Array()
		}
		ids := make([]msgpack.Value, 0, len(list))
		for _, m := range list {
			if b, err := hex.DecodeString(m.ID); err == nil {
				ids = append(ids, lxmf.Bin(b))
			}
		}
		return lxmf.Array(ids...)
	}
	if have.Kind == msgpack.KindArray && len(have.Array) > 0 {
		var ids []string
		for _, h := range have.Array {
			if b := binOf(h); len(b) == 32 {
				ids = append(ids, hex.EncodeToString(b))
			}
		}
		if n, err := s.store.DeleteLXMFHeld(ctx, tenantID, destHex, ids); err == nil && n > 0 {
			s.heldServed.Add(uint64(n))
		}
	}
	var limit int64
	if len(d.Array) >= 3 {
		if f, ok := d.Array[2].AsFloat(); ok && f > 0 {
			limit = int64(f * 1000)
		}
	}
	out := []msgpack.Value{}
	if want.Kind == msgpack.KindArray {
		var cumulative int64 = 24
		for _, w := range want.Array {
			b := binOf(w)
			if len(b) != 32 {
				continue
			}
			m, err := s.store.GetLXMFHeld(ctx, tenantID, hex.EncodeToString(b))
			if err != nil || m.Destination != destHex {
				continue
			}
			next := cumulative + int64(len(m.Data)) + 16
			if limit > 0 && next > limit {
				continue
			}
			cumulative = next
			out = append(out, lxmf.Bin(m.Data))
		}
	}
	return lxmf.Array(out...)
}

func binOf(v msgpack.Value) []byte {
	switch v.Kind {
	case msgpack.KindBin:
		return v.Bin
	case msgpack.KindStr:
		return []byte(v.Str)
	}
	return nil
}

// sweepHeld forgets expired messages and keeps every tenant under its cap.
func (s *Service) sweepHeld(ctx context.Context) {
	s.mu.Lock()
	tenants := make([]string, 0, len(s.routers))
	for tid, tr := range s.routers {
		if tr.pn != nil {
			tenants = append(tenants, tid)
		}
	}
	s.mu.Unlock()
	for _, tid := range tenants {
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if n, err := s.store.PruneLXMFHeld(sctx, tid, s.now().Add(-lxmf.MessageExpiry), s.cfg.PropagationMaxBytes); err != nil {
			slog.Warn("lxmf: propagation sweep failed", "tenant", tid, "error", err)
		} else if n > 0 {
			slog.Info("lxmf: propagation store swept", "tenant", tid, "removed", n)
		}
		cancel()
	}
}
