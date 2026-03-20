package reticulum

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// AstrocastInterface implements Interface for Astrocast LEO satellite transport.
// RX: Astrocast MO webhook delivers packets via OnReceive.
// TX: sends via constellation backend REST API.
type AstrocastInterface struct {
	mu        sync.RWMutex
	sender    SatelliteSender
	handler   PacketHandler
	available bool
}

// NewAstrocastInterface creates an Astrocast Reticulum transport interface.
func NewAstrocastInterface(sender SatelliteSender) *AstrocastInterface {
	return &AstrocastInterface{
		sender:    sender,
		available: sender != nil,
	}
}

func (a *AstrocastInterface) Name() InterfaceType { return IfaceAstrocast }

func (a *AstrocastInterface) Cost() float64 {
	if a.sender != nil {
		return a.sender.CostPerMessage()
	}
	return InterfaceCost(IfaceAstrocast)
}

func (a *AstrocastInterface) MTU() int {
	if a.sender != nil {
		return a.sender.MaxPayload()
	}
	return 160 // Astrocast default
}

func (a *AstrocastInterface) Send(ctx context.Context, destID string, packet []byte) error {
	if a.sender == nil {
		return fmt.Errorf("astrocast: no sender configured")
	}
	if len(packet) > a.MTU() {
		return fmt.Errorf("astrocast: packet %d bytes exceeds MTU %d", len(packet), a.MTU())
	}
	slog.Debug("reticulum: sending packet via astrocast", "dest", destID, "size", len(packet))
	return a.sender.Send(ctx, destID, packet)
}

func (a *AstrocastInterface) IsAvailable() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.available && a.sender != nil
}

// SetAvailable updates the availability status.
func (a *AstrocastInterface) SetAvailable(avail bool) {
	a.mu.Lock()
	a.available = avail
	a.mu.Unlock()
}

// SetHandler registers a callback for inbound Reticulum packets.
func (a *AstrocastInterface) SetHandler(h PacketHandler) {
	a.mu.Lock()
	a.handler = h
	a.mu.Unlock()
}

// OnReceive dispatches an inbound packet from the Astrocast MO webhook.
func (a *AstrocastInterface) OnReceive(raw []byte) {
	a.mu.RLock()
	h := a.handler
	a.mu.RUnlock()

	if h == nil {
		slog.Warn("reticulum: astrocast packet received but no handler registered", "size", len(raw))
		return
	}
	slog.Debug("reticulum: astrocast MO packet received", "size", len(raw))
	h(IfaceAstrocast, raw)
}
