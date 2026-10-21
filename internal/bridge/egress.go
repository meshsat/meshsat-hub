package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/meshsat/meshsat-hub/internal/codec"
	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// Egress formats a bridge declares for a satellite interface (HeMB-NG S3-2).
const (
	EgressSealed = "sealed"
	EgressPlain  = "plain"
)

// declaredFormat is the format an interface entry of a birth declares: its
// egress_format, or "sealed" from the older egress_sealed flag; "" when the
// entry declares nothing (an older bridge, a phone).
func declaredFormat(iface protocol.InterfaceInfo) string {
	switch iface.EgressFormat {
	case EgressSealed, EgressPlain:
		return iface.EgressFormat
	}
	if iface.EgressSealed {
		return EgressSealed
	}
	return ""
}

// egressStore is what EgressDeclarations needs at least: which bridge owns a
// modem, and that bridge's last birth.
type egressStore interface {
	DeviceBridgeID(ctx context.Context, tenantID, imei string) (string, error)
	GetBridge(ctx context.Context, tenantID, bridgeID string) (*store.Bridge, error)
}

// egressRecords is the durable record of declarations per modem, which the
// birth subscriber writes (store.Store has it).
type egressRecords interface {
	DeviceEgress(ctx context.Context, tenantID, imei string) (bridgeID, format string, err error)
}

// bridgeEgress is what a bridge declared per modem, with the stamp of the
// birth that declared it (store.Store has it).
type bridgeEgress interface {
	BridgeEgressEntry(ctx context.Context, tenantID, bridgeID, imei string) (store.EgressDecl, error)
}

// pendingSource is the declarations in births a subscriber in this process
// took and has not written in full (Subscriber.PendingEgressEntry).
type pendingSource interface {
	PendingEgressEntry(tenantID, imei string) (bridgeID string, e store.EgressDecl, ok bool)
}

// EgressDeclarations answers what the bridge that owns a modem declared about
// the modem's satellite payloads. It implements codec.EgressDeclarations.
//
// A declaration lives per bridge and modem. The birth subscriber stores it
// when a birth names the modem's IMEI with a format, ordered by the birth's
// own timestamp, and a birth without the IMEI or without the gateway leaves
// it alone: a restart before the 9704 has told its IMEI does not undo it. It
// holds while the bridge that declared it owns the modem; a modem that moves
// to another bridge is read by that bridge's declaration, or as undeclared.
// An explicit "plain" replaces "sealed".
//
// Lookup order, newest word first:
//  1. a declaration in a birth this process took and has not written in
//     full (SetPending), unless the bridge's stored declaration is newer
//     (another replica wrote a later birth). A held birth that links the
//     modem to a bridge other than its owner is unresolved until written:
//     DeclarationUnavailable;
//  2. the owner bridge's stored declaration for the modem;
//  3. the per-modem record (devices.egress_format);
//  4. the owner's last birth (one saved before either of the above existed);
//  5. what this process last confirmed for the modem.
//
// A failed lookup with nothing confirmed is DeclarationUnavailable, never
// Undeclared: an unknown is not a plain modem. While lookups fail, a held
// "sealed" still answers sealed, and a held "plain" is not taken on trust,
// since a newer stored word cannot be checked.
type EgressDeclarations struct {
	store egressStore

	mu        sync.Mutex
	pending   pendingSource
	confirmed map[string]confirmedEgress // tenant + "/" + imei
}

// SetPending gives the reader the births the subscriber in this process
// holds.
func (d *EgressDeclarations) SetPending(p pendingSource) {
	d.mu.Lock()
	d.pending = p
	d.mu.Unlock()
}

func (d *EgressDeclarations) held(tenantID, imei string) (string, store.EgressDecl, bool) {
	d.mu.Lock()
	p := d.pending
	d.mu.Unlock()
	if p == nil {
		return "", store.EgressDecl{}, false
	}
	return p.PendingEgressEntry(tenantID, imei)
}

type confirmedEgress struct {
	owner  string
	format string
}

// NewEgressDeclarations reads declarations from s.
func NewEgressDeclarations(s egressStore) *EgressDeclarations {
	return &EgressDeclarations{store: s, confirmed: map[string]confirmedEgress{}}
}

// Sealed reports whether the modem's owning bridge declared its chain
// sealing.
func (d *EgressDeclarations) Sealed(ctx context.Context, tenantID, imei string) bool {
	return d.Declaration(ctx, tenantID, imei) == codec.DeclaredSealed
}

// Declaration implements codec.EgressDeclarations.
func (d *EgressDeclarations) Declaration(ctx context.Context, tenantID, imei string) codec.Declaration {
	if d == nil || d.store == nil || imei == "" {
		return codec.Undeclared
	}
	key := tenantID + "/" + imei
	by, held, isHeld := d.held(tenantID, imei)
	owner, err := d.store.DeviceBridgeID(ctx, tenantID, imei)
	if err != nil {
		if isHeld && held.Format == EgressSealed {
			return codec.DeclaredSealed
		}
		return d.unavailable(key, "", "owner", err)
	}
	if isHeld {
		return d.fromHeld(ctx, key, tenantID, imei, owner, by, held)
	}
	if owner == "" {
		return codec.Undeclared
	}
	if be, ok := d.store.(bridgeEgress); ok {
		e, err := be.BridgeEgressEntry(ctx, tenantID, owner, imei)
		if err != nil {
			return d.unavailable(key, owner, "bridge declarations", err)
		}
		if e.Format != "" {
			return d.confirm(key, owner, e.Format)
		}
	}
	if recs, ok := d.store.(egressRecords); ok {
		by, format, err := recs.DeviceEgress(ctx, tenantID, imei)
		if err != nil {
			return d.unavailable(key, owner, "record", err)
		}
		if by == owner && format != "" {
			return d.confirm(key, owner, format)
		}
	}
	b, err := d.store.GetBridge(ctx, tenantID, owner)
	if err != nil {
		return d.unavailable(key, owner, "birth", err)
	}
	if b != nil && b.LastBirth != "" {
		var birth protocol.BridgeBirth
		if err := json.Unmarshal([]byte(b.LastBirth), &birth); err != nil {
			return d.unavailable(key, owner, "birth", err)
		}
		for _, iface := range birth.Interfaces {
			if iface.IMEI == imei {
				if format := declaredFormat(iface); format != "" {
					return d.confirm(key, owner, format)
				}
			}
		}
	}
	if c, ok := d.lastConfirmed(key); ok && c.owner == owner {
		return asDeclaration(c.format)
	}
	return codec.Undeclared
}

// fromHeld answers from a declaration in a birth this process holds: the
// newest word this process has on the modem, unless the declaring bridge's
// stored one is newer. A held birth of a bridge other than the modem's owner
// carries a link not yet written; which bridge's word applies is unresolved
// until it is, so the payload is kept opaque, never guessed at.
func (d *EgressDeclarations) fromHeld(ctx context.Context, key, tenantID, imei, owner, by string, held store.EgressDecl) codec.Declaration {
	if owner != "" && owner != by {
		slog.Warn("bridge: a held birth links the modem to another bridge; the payload is kept opaque until it is written",
			"device", key, "owner", owner, "held_by", by)
		return codec.DeclarationUnavailable
	}
	if be, ok := d.store.(bridgeEgress); ok {
		e, err := be.BridgeEgressEntry(ctx, tenantID, by, imei)
		if err != nil {
			if held.Format == EgressSealed {
				return codec.DeclaredSealed
			}
			return d.unavailable(key, owner, "bridge declarations", err)
		}
		if e.Format != "" && e.Stamp > held.Stamp {
			return d.confirm(key, by, e.Format)
		}
	}
	return asDeclaration(held.Format)
}

func (d *EgressDeclarations) confirm(key, owner, format string) codec.Declaration {
	d.mu.Lock()
	d.confirmed[key] = confirmedEgress{owner: owner, format: format}
	d.mu.Unlock()
	return asDeclaration(format)
}

func (d *EgressDeclarations) lastConfirmed(key string) (confirmedEgress, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.confirmed[key]
	return c, ok
}

// unavailable answers a failed lookup: what this process last confirmed for
// the modem (with the same owner, when the owner is known), else
// DeclarationUnavailable.
func (d *EgressDeclarations) unavailable(key, owner, what string, err error) codec.Declaration {
	if c, ok := d.lastConfirmed(key); ok && (owner == "" || c.owner == owner) {
		slog.Warn("bridge: egress declaration lookup failed, using the last one confirmed", "device", key, "lookup", what, "format", c.format, "error", err)
		return asDeclaration(c.format)
	}
	slog.Warn("bridge: egress declaration lookup failed; the payload is kept opaque, not guessed at", "device", key, "lookup", what, "error", err)
	return codec.DeclarationUnavailable
}

func asDeclaration(format string) codec.Declaration {
	switch format {
	case EgressSealed:
		return codec.DeclaredSealed
	case EgressPlain:
		return codec.DeclaredPlain
	}
	return codec.Undeclared
}
