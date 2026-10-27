package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/meshsat/meshsat-hub/internal/config"
	"github.com/meshsat/meshsat-hub/internal/reticulum"
	"github.com/meshsat/meshsat-hub/internal/rns"
	"github.com/meshsat/meshsat-hub/internal/rnsnode"
	"github.com/meshsat/meshsat-hub/internal/rnswire"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// tcpLeg is what main.go needs from the Reticulum TCP leg whichever layer
// owns it: the legacy TCPInterface when the node is off, rnsnode.LegacyTCP
// when the node is on (MESHSAT-1555).
type tcpLeg interface {
	reticulum.Interface
	ClientCount() int
}

// mqttLeg adapts the legacy Reticulum MQTT interface to the node's
// transmitter: the node publishes on the same topic the bridges read.
type mqttLeg struct{ iface *reticulum.MQTTInterface }

func (m mqttLeg) Publish(raw []byte) error { return m.iface.Send(context.Background(), "", raw) }
func (m mqttLeg) Connected() bool          { return m.iface.IsAvailable() }

// buildRNSNode assembles the upstream-compatible node on the Hub's legacy
// identity (so the transport id and the meshsat.hub hash do not change for
// the kits), with the legacy handler as the fall-through for frames the node
// declines and the legacy router as the mirror for every accepted announce,
// which keeps the bridge route tables and route hints working as before.
func buildRNSNode(cfg config.Config, hubIdentity *reticulum.HubIdentity, dataStore store.Store,
	mqttIface *reticulum.MQTTInterface, legacyHandler reticulum.PacketHandler, legacyRouter *reticulum.Router,
	onNode func(*rns.Node), onNodeStopped func()) (*rnsnode.Service, error) {
	legacy := hubIdentity.Identity()
	id, err := rnswire.LoadIdentity(legacy.EncryptionPrivateBytes(), legacy.SigningPrivateBytes())
	if err != nil {
		return nil, fmt.Errorf("rns: hub identity: %w", err)
	}
	ncfg := rnsnode.Config{
		Identity: id,
		Store:    dataStore,
		PathTTL:  time.Duration(cfg.RNSPathTTLHours) * time.Hour,
		// The public listener's classification into the main node and the
		// Private tenants' own nodes (MESHSAT-1722).
		IFACTriesPerSecond:  cfg.RNSIFACTriesPerSecond,
		UnclassifiedTimeout: cfg.RNSUnclassifiedTimeout,
		BridgeListenAddr:    cfg.ReticulumTCPAddr,
		PublicListenAddr:    cfg.RNSPublicTCPAddr,
		OnDeclinedTCP:       func(raw []byte) { legacyHandler(reticulum.IfaceTCP, raw) },
		OnNode:              onNode,
		OnNodeStopped:       onNodeStopped,
		OnAnnounce: func(_ *rnswire.Announce, raw []byte, src rnsnode.Source) {
			ann, err := reticulum.UnmarshalAnnouncePacket(raw)
			if err != nil {
				return
			}
			iface := reticulum.IfaceTCP
			if src == rnsnode.SourceMQTT {
				iface = reticulum.IfaceMQTT
			}
			legacyRouter.ProcessAnnounce(ann, iface)
		},
	}
	if mqttIface != nil {
		ncfg.MQTT = mqttLeg{iface: mqttIface}
	}
	if cfg.RNSPublicIFACNetname != "" || cfg.RNSPublicIFACNetkey != "" {
		f, err := rnswire.NewIFAC(cfg.RNSPublicIFACNetname, cfg.RNSPublicIFACNetkey, rnswire.DefaultIFACSize)
		if err != nil {
			return nil, fmt.Errorf("rns: public interface access code: %w", err)
		}
		ncfg.PublicIFAC = f
		slog.Info("rns: interface access codes enabled on the public listener")
	}
	return rnsnode.New(ncfg), nil
}
