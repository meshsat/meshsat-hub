package bridge

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/meshsat/meshsat-hub/internal/protocol"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// egressStore is what EgressDeclarations reads: which bridge owns a modem,
// and that bridge's last birth.
type egressStore interface {
	DeviceBridgeID(ctx context.Context, tenantID, imei string) (string, error)
	GetBridge(ctx context.Context, tenantID, bridgeID string) (*store.Bridge, error)
}

// EgressDeclarations reads, from the last birth of the bridge that owns a
// modem, whether the satellite interface on that modem declared its send
// chain sealing (InterfaceInfo.EgressSealed). It implements
// codec.EgressDeclarations. The birth is the bridge's own word, retained and
// stored, so it holds while the bridge is off the network and its messages
// come by satellite. A modem no bridge owns, a bridge without a stored birth
// or a birth without the declaration answers false: the payload is read as
// before.
type EgressDeclarations struct {
	store egressStore
}

// NewEgressDeclarations reads declarations from s.
func NewEgressDeclarations(s egressStore) *EgressDeclarations {
	return &EgressDeclarations{store: s}
}

// Sealed implements codec.EgressDeclarations.
func (d *EgressDeclarations) Sealed(ctx context.Context, tenantID, imei string) bool {
	if d == nil || d.store == nil || imei == "" {
		return false
	}
	bridgeID, err := d.store.DeviceBridgeID(ctx, tenantID, imei)
	if err != nil || bridgeID == "" {
		return false
	}
	b, err := d.store.GetBridge(ctx, tenantID, bridgeID)
	if err != nil || b == nil || b.LastBirth == "" {
		return false
	}
	var birth protocol.BridgeBirth
	if err := json.Unmarshal([]byte(b.LastBirth), &birth); err != nil {
		slog.Debug("bridge: stored birth unreadable, no egress declaration", "bridge", bridgeID, "error", err)
		return false
	}
	for _, iface := range birth.Interfaces {
		if iface.IMEI == imei {
			return iface.EgressSealed
		}
	}
	return false
}
