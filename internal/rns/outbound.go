package rns

import (
	"crypto/ecdh"
	"errors"
	"fmt"
	"time"

	"github.com/meshsat/meshsat-hub/internal/rnswire"
)

// ErrUnknownDestination is returned when no announce has been seen for a
// destination, so it cannot be encrypted to.
var ErrUnknownDestination = errors.New("rns: destination identity unknown")

// outbound sends a locally originated non-link packet (Transport.outbound):
// along the path when one is known (HEADER_2 insertion beyond one hop),
// otherwise broadcast on every floodable interface. Returns the interface(s)
// used.
func (n *Node) outbound(raw []byte, dest [HashLen]byte, destType byte) ([]string, error) {
	hdr, err := rnswire.UnmarshalHeader(raw)
	if err != nil {
		return nil, err
	}
	if hdr.PacketType != rnswire.PacketAnnounce && destType != rnswire.DestPlain && destType != rnswire.DestGroup {
		if e := n.paths.Get(dest); e != nil && n.now().Before(e.Expires) {
			out := raw
			if !e.Direct() {
				out = rnswire.RewriteForTransport(raw, e.NextHop)
			}
			e.Timestamp = n.now()
			return []string{e.Iface}, n.transmit(e.Iface, out)
		}
	}
	ifaces := n.tx.Floodable()
	if len(ifaces) == 0 {
		return nil, ErrNoPath
	}
	for _, id := range ifaces {
		n.transmit(id, raw)
	}
	return ifaces, nil
}

// HopsTo returns the known hop count to a destination or -1.
func (n *Node) HopsTo(dest [HashLen]byte) int { return n.paths.Hops(dest) }

// NextHopInterface returns the interface a destination is reached on.
func (n *Node) NextHopInterface(dest [HashLen]byte) string {
	if e := n.paths.Get(dest); e != nil {
		return e.Iface
	}
	return ""
}

// SendData encrypts plaintext for a remote SINGLE destination and sends it
// as one DATA packet (Packet with destination.encrypt). The destination
// must be known from an announce. The returned Receipt resolves when the
// destination's proof arrives.
func (n *Node) SendData(dest [HashLen]byte, plaintext []byte, wantReceipt bool) (*Receipt, error) {
	k := n.Recall(dest)
	if k == nil || k.Identity == nil {
		return nil, ErrUnknownDestination
	}
	if len(plaintext) > rnswire.EncryptedMDU {
		return nil, fmt.Errorf("rns: %d bytes exceed the single-packet limit of %d", len(plaintext), rnswire.EncryptedMDU)
	}
	var ratchet *ecdh.PublicKey
	if len(k.Ratchet) == rnswire.RatchetKeyLen && n.now().Before(k.RatchetAt.Add(30*24*time.Hour)) {
		ratchet, _ = ecdh.X25519().NewPublicKey(k.Ratchet)
	}
	ct, err := k.Identity.Encrypt(plaintext, ratchet)
	if err != nil {
		return nil, err
	}
	h := &rnswire.Header{HeaderType: rnswire.HeaderType1, DestType: rnswire.DestSingle, PacketType: rnswire.PacketData,
		DestHash: dest, Context: rnswire.ContextNone, Data: ct}
	raw := h.Marshal()
	var r *Receipt
	if wantReceipt {
		hops := n.paths.Hops(dest)
		if hops < 1 {
			hops = 1
		}
		r = &Receipt{Hash: rnswire.PacketHash(raw), Dest: dest, SentAt: n.now(),
			Timeout: n.now().Add(time.Duration(rnswire.DefaultPerHopTimeoutSec*hops)*time.Second + 6*time.Second),
			sigPub:  k.Identity.SigningPublicKey()}
		n.addReceipt(r)
	}
	if _, err := n.outbound(raw, dest, rnswire.DestSingle); err != nil {
		if r != nil {
			n.mu.Lock()
			delete(n.receipts, r.Hash)
			n.mu.Unlock()
		}
		return nil, err
	}
	return r, nil
}

// SendRaw transmits an already-built packet for dest along the path rules.
func (n *Node) SendRaw(raw []byte, dest [HashLen]byte, destType byte) error {
	_, err := n.outbound(raw, dest, destType)
	return err
}
