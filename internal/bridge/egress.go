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

// bridgeEgress is what a bridge declared per modem, merged in the statement
// that saves its birth (store.Store has it).
type bridgeEgress interface {
	BridgeEgress(ctx context.Context, tenantID, bridgeID, imei string) (string, error)
}

// pendingSource is the declarations a subscriber in this process took from a
// birth and has not written in full (Subscriber.PendingEgress).
type pendingSource interface {
	PendingEgress(tenantID, imei string) (bridgeID, format string, ok bool)
}

// EgressDeclarations answers what the bridge that owns a modem declared about
// the modem's satellite payloads. It implements codec.EgressDeclarations.
//
// A declaration lives per modem. The birth subscriber records it when a birth
// names the modem's IMEI with a format, and a birth without the IMEI or
// without the gateway leaves it alone: a restart before the 9704 has told its
// IMEI does not undo it. It holds while the bridge that declared it owns the
// modem; a modem that moves to another bridge is read by that bridge's
// declaration, or as undeclared. An explicit "plain" replaces "sealed".
//
// Lookup order, newest word first:
//  1. a declaration this process took from a birth and has not written in
//     full (SetPending): a failed write never leaves an older one in charge;
//  2. the owner bridge's merged declarations, saved in the statement that
//     saves its birth, so they cannot disagree with it;
//  3. the per-modem record (devices.egress_format);
//  4. the owner's last birth (one saved before either of the above existed);
//  5. what this process last confirmed for the modem.
//
// A failed lookup with nothing confirmed is DeclarationUnavailable, never
// Undeclared: an unknown is not a plain modem.
type EgressDeclarations struct {
	store egressStore

	mu        sync.Mutex
	pending   pendingSource
	confirmed map[string]confirmedEgress // tenant + "/" + imei
}

// SetPending gives the reader the declarations the birth subscriber in this
// process holds pending.
func (d *EgressDeclarations) SetPending(p pendingSource) {
	d.mu.Lock()
	d.pending = p
	d.mu.Unlock()
}

func (d *EgressDeclarations) pendingFor(tenantID, imei, owner string) (string, bool) {
	d.mu.Lock()
	p := d.pending
	d.mu.Unlock()
	if p == nil {
		return "", false
	}
	by, format, ok := p.PendingEgress(tenantID, imei)
	if !ok || (owner != "" && by != owner) {
		return "", false
	}
	return format, true
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
	owner, err := d.store.DeviceBridgeID(ctx, tenantID, imei)
	if err != nil {
		if format, ok := d.pendingFor(tenantID, imei, ""); ok {
			return asDeclaration(format)
		}
		return d.unavailable(key, "", "owner", err)
	}
	if format, ok := d.pendingFor(tenantID, imei, owner); ok {
		return asDeclaration(format)
	}
	if owner == "" {
		return codec.Undeclared
	}
	if be, ok := d.store.(bridgeEgress); ok {
		format, err := be.BridgeEgress(ctx, tenantID, owner, imei)
		if err != nil {
			return d.unavailable(key, owner, "bridge declarations", err)
		}
		if format != "" {
			return d.confirm(key, owner, format)
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
