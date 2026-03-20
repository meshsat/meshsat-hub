package reticulum

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// GlobalstarInterface implements Interface for Globalstar LEO satellite transport.
// Globalstar has a 128-byte bidirectional payload; larger Reticulum packets
// (e.g. announces at ~163 bytes) require message chaining (2 segments).
// RX: Globalstar MO webhook delivers packets via OnReceive.
// TX: sends via REST API (placeholder until Globalstar SDK available).
type GlobalstarInterface struct {
	mu        sync.RWMutex
	sender    SatelliteSender
	handler   PacketHandler
	available bool
}

// NewGlobalstarInterface creates a Globalstar Reticulum transport interface.
func NewGlobalstarInterface(sender SatelliteSender) *GlobalstarInterface {
	return &GlobalstarInterface{
		sender:    sender,
		available: sender != nil,
	}
}

func (g *GlobalstarInterface) Name() InterfaceType { return IfaceGlobalstar }

func (g *GlobalstarInterface) Cost() float64 {
	if g.sender != nil {
		return g.sender.CostPerMessage()
	}
	return InterfaceCost(IfaceGlobalstar)
}

func (g *GlobalstarInterface) MTU() int {
	if g.sender != nil {
		return g.sender.MaxPayload()
	}
	return 128 // Globalstar base MTU
}

func (g *GlobalstarInterface) Send(ctx context.Context, destID string, packet []byte) error {
	if g.sender == nil {
		return fmt.Errorf("globalstar: no sender configured")
	}
	if len(packet) > g.MTU() {
		return fmt.Errorf("globalstar: packet %d bytes exceeds MTU %d (message chaining not yet implemented)", len(packet), g.MTU())
	}
	slog.Debug("reticulum: sending packet via globalstar", "dest", destID, "size", len(packet))
	return g.sender.Send(ctx, destID, packet)
}

func (g *GlobalstarInterface) IsAvailable() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.available && g.sender != nil
}

// SetAvailable updates the availability status.
func (g *GlobalstarInterface) SetAvailable(avail bool) {
	g.mu.Lock()
	g.available = avail
	g.mu.Unlock()
}

// SetHandler registers a callback for inbound Reticulum packets.
func (g *GlobalstarInterface) SetHandler(h PacketHandler) {
	g.mu.Lock()
	g.handler = h
	g.mu.Unlock()
}

// OnReceive dispatches an inbound packet from the Globalstar MO webhook.
func (g *GlobalstarInterface) OnReceive(raw []byte) {
	g.mu.RLock()
	h := g.handler
	g.mu.RUnlock()

	if h == nil {
		slog.Warn("reticulum: globalstar packet received but no handler registered", "size", len(raw))
		return
	}
	slog.Debug("reticulum: globalstar MO packet received", "size", len(raw))
	h(IfaceGlobalstar, raw)
}
