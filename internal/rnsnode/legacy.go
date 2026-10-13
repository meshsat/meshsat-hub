package rnsnode

import (
	"context"
	"errors"

	"github.com/meshsat/meshsat-hub/internal/reticulum"
)

// LegacyTCP presents the node's stunnel listener to the legacy Reticulum
// relay as its "tcp" interface. The legacy layer kept one TCP interface that
// broadcast every packet to every connected bridge; this keeps that shape
// for the MeshSat-private packets the node declines (time sync, custody,
// route hints), while the node itself names one connection per packet. The
// public listener is deliberately not part of it: a stock client never
// receives a MeshSat-private frame.
type LegacyTCP struct{ s *Service }

// NewLegacyTCP wraps the service for reticulum.Relay.RegisterInterface.
func NewLegacyTCP(s *Service) *LegacyTCP { return &LegacyTCP{s: s} }

// Name is the legacy interface type the relay keys on.
func (l *LegacyTCP) Name() reticulum.InterfaceType { return reticulum.IfaceTCP }

// Cost is zero: TCP is free.
func (l *LegacyTCP) Cost() float64 { return 0 }

// MTU is the Reticulum MTU the legacy relay checks against.
func (l *LegacyTCP) MTU() int { return reticulum.MTU }

// Send broadcasts a legacy packet to every bridge on the stunnel listener.
func (l *LegacyTCP) Send(_ context.Context, _ string, packet []byte) error {
	n, err := l.s.Broadcast(packet)
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("rnsnode: no bridge connected")
	}
	return nil
}

// IsAvailable is true while this replica is the leader and listening.
func (l *LegacyTCP) IsAvailable() bool { return l.s.Listening() }

// ClientCount is the number of bridges connected to the stunnel listener,
// which the legacy meshsat.hub announcer reads before announcing.
func (l *LegacyTCP) ClientCount() int { return l.s.BridgeCount() }
