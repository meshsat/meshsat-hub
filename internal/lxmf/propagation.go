package lxmf

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/meshsat/meshsat-hub/internal/msgpack"
	"github.com/meshsat/meshsat-hub/internal/rnswire"
)

// The LXMF propagation node protocol (LXMRouter.enable_propagation,
// LXMF 1.2.0), as far as a node that serves clients needs it. Peering
// between propagation nodes (the /offer handshake with a peering key) is not
// implemented: /offer answers ErrorNoAccess.

const (
	// PropagationName is the full destination name of the propagation
	// destination, app name and aspect, as DeliveryName is: a client computes
	// RNS.Destination.hash(identity, "lxmf", "propagation") and must land here.
	PropagationName = "lxmf.propagation"
	// OfferPath and GetPath are the request paths a propagation node serves.
	OfferPath = "/offer"
	GetPath   = "/get"

	// PNStampRounds is LXStamper.WORKBLOCK_EXPAND_ROUNDS_PN.
	PNStampRounds = 1000
	// StampSize is the trailing stamp on a message held by a propagation node.
	StampSize = 32

	// Upstream defaults (LXMRouter).
	DefaultPropagationCost     = 16  // PROPAGATION_COST
	DefaultPropagationCostFlex = 3   // PROPAGATION_COST_FLEX
	DefaultPeeringCost         = 18  // PEERING_COST
	DefaultTransferLimitKB     = 256 // PROPAGATION_LIMIT, per transfer to a client
	DefaultSyncLimitKB         = DefaultTransferLimitKB * 40
	DefaultDeliveryLimitKB     = 1000 // what a client accepts per transfer
	MessageExpiry              = 30 * 24 * time.Hour

	// Response codes a propagation node answers with (LXMPeer).
	ErrorNoIdentity   = 0xf0
	ErrorNoAccess     = 0xf1
	ErrorInvalidKey   = 0xf3
	ErrorInvalidData  = 0xf4
	ErrorInvalidStamp = 0xf5
	ErrorThrottled    = 0xf6
	ErrorNotFound     = 0xfd

	// Propagation node metadata keys in the announce (PN_META_*).
	pnMetaVersion  = 0x00
	pnMetaName     = 0x01
	pnMetaImplName = 0xFE

	// Propagated is the delivery method of a message that came through a
	// propagation node (LXMessage.PROPAGATED).
	Propagated Method = 3
)

// WorkblockRounds is LXStamper.stamp_workblock with a chosen expansion
// count: 3000 for a delivery stamp, 1000 for a propagation stamp, 25 for a
// peering key.
func WorkblockRounds(material []byte, rounds int) []byte {
	out := make([]byte, 0, rounds*256)
	for n := 0; n < rounds; n++ {
		salt := sha256.Sum256(append(append([]byte(nil), material...), Pack(Int(int64(n)))...))
		out = append(out, rnswire.HKDF(256, material, salt[:], nil)...)
	}
	return out
}

// TransientID is the propagation node's key for a message: SHA-256 over the
// message as the node holds it (destination hash and encrypted payload,
// without the stamp).
func TransientID(lxm []byte) [32]byte { return sha256.Sum256(lxm) }

// HeldMessage is one entry a client handed a propagation node.
type HeldMessage struct {
	TransientID [32]byte
	Destination [DestLen]byte
	// Data is the message without its stamp: what /get returns.
	Data []byte
	// Stamp is the trailing 32 bytes; StampValue its leading zero bits.
	Stamp      []byte
	StampValue int
}

// ValidatePNStamp is LXStamper.validate_pn_stamp: the trailing 32 bytes are
// the stamp over the transient id's workblock; ok is false when the data is
// too short or the stamp does not reach minCost. minCost 0 accepts any
// stamp, which is also what a client attaches when the node announced no
// cost.
func ValidatePNStamp(transientData []byte, minCost int) (*HeldMessage, bool) {
	if len(transientData) <= Overhead+StampSize {
		return nil, false
	}
	lxm := transientData[:len(transientData)-StampSize]
	stamp := transientData[len(transientData)-StampSize:]
	id := TransientID(lxm)
	wb := WorkblockRounds(id[:], PNStampRounds)
	h := sha256.Sum256(append(append([]byte(nil), wb...), stamp...))
	value := StampValue(wb, stamp, h)
	if minCost > 0 && value < minCost {
		return nil, false
	}
	m := &HeldMessage{TransientID: id, Data: append([]byte(nil), lxm...), Stamp: append([]byte(nil), stamp...), StampValue: value}
	copy(m.Destination[:], lxm[:DestLen])
	return m, true
}

// GeneratePNStamp makes a propagation stamp for a message at cost, for a
// client-side send or a test; cost 0 is a random stamp, as upstream attaches.
func GeneratePNStamp(lxm []byte, cost int) []byte {
	id := TransientID(lxm)
	wb := WorkblockRounds(id[:], PNStampRounds)
	stamp, _ := searchStamp(context.Background(), wb, cost)
	return stamp
}

// PropagationAnnounce is what a propagation node puts in its announce.
type PropagationAnnounce struct {
	Timebase        int64
	Enabled         bool
	TransferLimitKB int
	SyncLimitKB     int
	StampCost       int
	StampCostFlex   int
	PeeringCost     int
	Name            string
	Version         string
}

// AppData packs the announce (LXMRouter.get_propagation_node_app_data).
func (p PropagationAnnounce) AppData() []byte {
	meta := []msgpack.KV{
		{Key: Int(pnMetaImplName), Val: Str("meshsat-hub")},
		{Key: Int(pnMetaVersion), Val: Str(p.Version)},
	}
	if p.Name != "" {
		meta = append(meta, msgpack.KV{Key: Int(pnMetaName), Val: Bin([]byte(p.Name))})
	}
	return Pack(Array(
		Bool(false), // legacy propagation node support
		Int(p.Timebase),
		Bool(p.Enabled),
		Int(int64(p.TransferLimitKB)),
		Int(int64(p.SyncLimitKB)),
		Array(Int(int64(p.StampCost)), Int(int64(p.StampCostFlex)), Int(int64(p.PeeringCost))),
		MapOf(meta...),
	))
}

// ParsePropagationAnnounce reads a propagation node's announce app data.
func ParsePropagationAnnounce(appData []byte) (*PropagationAnnounce, error) {
	v, _, err := msgpack.UnpackValue(appData)
	if err != nil || v.Kind != msgpack.KindArray || len(v.Array) < 6 {
		return nil, errors.New("lxmf: not a propagation node announce")
	}
	a := &PropagationAnnounce{}
	a.Timebase, _ = v.Array[1].AsInt()
	a.Enabled = v.Array[2].Kind == msgpack.KindBool && v.Array[2].Bool
	tl, _ := v.Array[3].AsInt()
	sl, _ := v.Array[4].AsInt()
	a.TransferLimitKB, a.SyncLimitKB = int(tl), int(sl)
	if c := v.Array[5]; c.Kind == msgpack.KindArray && len(c.Array) >= 3 {
		sc, _ := c.Array[0].AsInt()
		fl, _ := c.Array[1].AsInt()
		pc, _ := c.Array[2].AsInt()
		a.StampCost, a.StampCostFlex, a.PeeringCost = int(sc), int(fl), int(pc)
	}
	if len(v.Array) > 6 && v.Array[6].Kind == msgpack.KindMap {
		for _, kv := range v.Array[6].Map {
			k, _ := kv.Key.AsInt()
			switch k {
			case pnMetaName:
				a.Name = string(binOrStr(kv.Val))
			case pnMetaVersion:
				a.Version = string(binOrStr(kv.Val))
			}
		}
	}
	return a, nil
}

// PropagationPacked is what a client sends a propagation node: msgpack
// [timebase, [message_with_stamp, ...]].
func PropagationPacked(timebase float64, messages [][]byte) []byte {
	items := make([]msgpack.Value, 0, len(messages))
	for _, m := range messages {
		items = append(items, Bin(m))
	}
	return Pack(Array(Float(timebase), Array(items...)))
}

// UnpackPropagation reads what a client sent: the messages, each still
// carrying its stamp.
func UnpackPropagation(data []byte) (timebase float64, messages [][]byte, err error) {
	v, _, err := msgpack.UnpackValue(data)
	if err != nil || v.Kind != msgpack.KindArray || len(v.Array) != 2 || v.Array[1].Kind != msgpack.KindArray {
		return 0, nil, errors.New("lxmf: not a propagation transfer")
	}
	timebase, _ = v.Array[0].AsFloat()
	for _, item := range v.Array[1].Array {
		b := binOrStr(item)
		if b == nil {
			return 0, nil, errors.New("lxmf: propagation transfer item is not bytes")
		}
		messages = append(messages, b)
	}
	return timebase, messages, nil
}
