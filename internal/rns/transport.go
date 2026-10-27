package rns

import (
	"time"

	"log/slog"

	"github.com/meshsat/meshsat-hub/internal/rnswire"
)

// linkEntry is one row of Transport.link_table: a link request we forwarded.
type linkEntry struct {
	timestamp     time.Time
	nextHop       [HashLen]byte
	nextHopIface  string
	remainingHops int
	receivedIface string
	takenHops     int
	destHash      [HashLen]byte
	validated     bool
	proofTimeout  time.Time
}

// reverseEntry is one row of Transport.reverse_table: a packet we forwarded
// whose proof must travel back.
type reverseEntry struct {
	receivedIface string
	outboundIface string
	timestamp     time.Time
}

// forwardTransport handles a HEADER_2 packet addressed to this transport
// instance (Transport.inbound, "packet.transport_id == identity.hash").
// Returns false when no path is known.
func (n *Node) forwardTransport(pkt *Packet) bool {
	h := pkt.Hdr
	entry := n.paths.Get(h.DestHash)
	if entry == nil {
		slog.Debug("rns: packet in transport, no path to destination, dropping", "dest", hexh(h.DestHash))
		return false
	}
	if n.crossesLeaf(pkt.Iface, entry.Iface) {
		return true
	}
	now := n.now()
	var out []byte
	switch {
	case !entry.Direct():
		out = rnswire.RewriteForTransport(pkt.Raw, entry.NextHop)
	default:
		out = rnswire.StripTransport(pkt.Raw)
	}
	out[1] = h.Hops

	if h.PacketType == rnswire.PacketLinkRequest {
		// Clamp the signalled link MTU to the smaller of both interfaces.
		if len(h.Data) == rnswire.LinkECPubSize+rnswire.LinkMTUSize {
			pathMTU, mode := rnswire.ParseSignalling(h.Data[rnswire.LinkECPubSize:])
			nhMTU := n.tx.HWMTU(entry.Iface)
			phMTU := n.tx.HWMTU(pkt.Iface)
			if nhMTU == 0 {
				out = out[:len(out)-rnswire.LinkMTUSize]
			} else {
				m := pathMTU
				if nhMTU < m {
					m = nhMTU
				}
				if phMTU > 0 && phMTU < m {
					m = phMTU
				}
				if m != pathMTU {
					s := rnswire.SignallingBytes(m, mode)
					copy(out[len(out)-rnswire.LinkMTUSize:], s[:])
				}
			}
		}
		linkID, err := rnswire.LinkIDFromRequestPacket(pkt.Raw)
		if err != nil {
			return true
		}
		rem := entry.Hops
		if rem < 1 {
			rem = 1
		}
		n.mu.Lock()
		n.linkTable[linkID] = &linkEntry{
			timestamp:     now,
			nextHop:       entry.NextHop,
			nextHopIface:  entry.Iface,
			remainingHops: entry.Hops,
			receivedIface: pkt.Iface,
			takenHops:     int(h.Hops),
			destHash:      h.DestHash,
			proofTimeout:  now.Add(time.Duration(rnswire.DefaultPerHopTimeoutSec*rem) * time.Second),
		}
		n.mu.Unlock()
	} else {
		n.mu.Lock()
		n.reverseTable[pkt.TruncatedHash()] = &reverseEntry{receivedIface: pkt.Iface, outboundIface: entry.Iface, timestamp: now}
		n.mu.Unlock()
	}
	entry.Timestamp = now
	n.count(func(s *Stats) { s.Forwarded++ })
	_ = n.transmit(entry.Iface, out)
	return true
}

// forwardHeader1 is the MeshSat extension: a HEADER_1 packet for a remote
// destination arriving from an interface whose peers cannot address a
// transport (the Meshtastic mesh) is inserted into transport here.
func (n *Node) forwardHeader1(pkt *Packet) bool {
	if !n.cfg.ForwardHeader1From[pkt.Iface] {
		return false
	}
	h := pkt.Hdr
	entry := n.paths.Get(h.DestHash)
	if entry == nil || entry.Iface == pkt.Iface {
		return false
	}
	if n.crossesLeaf(pkt.Iface, entry.Iface) {
		return true
	}
	var out []byte
	if !entry.Direct() {
		out = rnswire.RewriteForTransport(pkt.Raw, entry.NextHop)
	} else {
		out = append([]byte(nil), pkt.Raw...)
	}
	out[1] = h.Hops
	n.mu.Lock()
	if h.PacketType == rnswire.PacketLinkRequest {
		if linkID, err := rnswire.LinkIDFromRequestPacket(pkt.Raw); err == nil {
			n.linkTable[linkID] = &linkEntry{timestamp: n.now(), nextHop: entry.NextHop, nextHopIface: entry.Iface,
				remainingHops: entry.Hops, receivedIface: pkt.Iface, takenHops: int(h.Hops), destHash: h.DestHash,
				proofTimeout: n.now().Add(time.Duration(rnswire.DefaultPerHopTimeoutSec*max(1, entry.Hops)) * time.Second)}
		}
	} else {
		n.reverseTable[pkt.TruncatedHash()] = &reverseEntry{receivedIface: pkt.Iface, outboundIface: entry.Iface, timestamp: n.now()}
	}
	n.mu.Unlock()
	n.count(func(s *Stats) { s.Forwarded++ })
	_ = n.transmit(entry.Iface, out)
	return true
}

// linkTransit repeats packets belonging to a link we carry.
func (n *Node) linkTransit(pkt *Packet) bool {
	h := pkt.Hdr
	n.mu.Lock()
	e, ok := n.linkTable[h.DestHash]
	if !ok {
		n.mu.Unlock()
		return false
	}
	if !e.validated {
		n.mu.Unlock()
		return true // link packet before validation: drop
	}
	outIface := ""
	hops := int(h.Hops)
	if e.nextHopIface == e.receivedIface {
		if hops == e.remainingHops || hops == e.takenHops {
			outIface = e.nextHopIface
		}
	} else if pkt.Iface == e.nextHopIface {
		if hops == e.remainingHops {
			outIface = e.receivedIface
		}
	} else if pkt.Iface == e.receivedIface {
		if hops == e.takenHops {
			outIface = e.nextHopIface
		}
	}
	if outIface == "" {
		n.mu.Unlock()
		return true
	}
	n.hashlist[pkt.Hash] = struct{}{}
	e.timestamp = n.now()
	n.stats.LinkTransit++
	n.mu.Unlock()
	_ = n.transmit(outIface, pkt.Raw)
	return true
}

// lrProofTransit validates and forwards a link request proof for a link in
// the link table. Returns false when the proof is not for a transit link.
func (n *Node) lrProofTransit(pkt *Packet) bool {
	h := pkt.Hdr
	n.mu.Lock()
	e, ok := n.linkTable[h.DestHash]
	if !ok {
		n.mu.Unlock()
		return false
	}
	if int(h.Hops) != e.remainingHops || pkt.Iface != e.nextHopIface {
		n.mu.Unlock()
		slog.Debug("rns: link proof hop or interface mismatch in transit", "link", hexh(h.DestHash), "hops", int(h.Hops), "want", e.remainingHops)
		return true
	}
	known := n.identities[e.destHash]
	n.mu.Unlock()
	lp, err := rnswire.UnmarshalRNSLinkProof(h.Data)
	if err != nil || known == nil || known.Identity == nil {
		return true
	}
	signed := rnswire.LinkProofSignedData(h.DestHash, lp.EphPub, known.Identity.SigningPublicKey(), lp.MTU, lp.Mode)
	if !lp.Signalled {
		signed = signed[:len(signed)-rnswire.LinkMTUSize]
	}
	if !rnswire.VerifySignature(known.Identity.SigningPublicKey(), signed, lp.Signature) {
		slog.Debug("rns: invalid link request proof in transport", "link", hexh(h.DestHash))
		return true
	}
	n.mu.Lock()
	e.validated = true
	e.timestamp = n.now()
	n.mu.Unlock()
	_ = n.transmit(e.receivedIface, pkt.Raw)
	return true
}

// proofTransit forwards a packet proof back along the reverse table.
func (n *Node) proofTransit(pkt *Packet) {
	h := pkt.Hdr
	n.mu.Lock()
	e, ok := n.reverseTable[h.DestHash]
	if ok {
		delete(n.reverseTable, h.DestHash)
	}
	n.mu.Unlock()
	if !ok {
		return
	}
	if pkt.Iface != e.outboundIface {
		return
	}
	_ = n.transmit(e.receivedIface, pkt.Raw)
}

// expireTables sweeps the link, reverse and path tables.
func (n *Node) expireTables(now time.Time) {
	n.mu.Lock()
	for k, e := range n.reverseTable {
		if now.After(e.timestamp.Add(rnswire.ReverseTimeoutSec * time.Second)) {
			delete(n.reverseTable, k)
		}
	}
	for k, e := range n.linkTable {
		if !e.validated && now.After(e.proofTimeout) {
			delete(n.linkTable, k)
		} else if now.After(e.timestamp.Add(rnswire.LinkTimeoutSec * time.Second)) {
			delete(n.linkTable, k)
		}
	}
	n.mu.Unlock()
	n.paths.Expire(now)
}
