package reticulum

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// SatelliteSender is the subset of constellation.Backend needed by the
// Iridium interface. Using an interface avoids importing the constellation
// package directly.
type SatelliteSender interface {
	Send(ctx context.Context, deviceID string, payload []byte) error
	IsAvailable(ctx context.Context) bool
	MaxPayload() int
	CostPerMessage() float64
}

// IridiumInterface implements Interface for Iridium SBD transport.
// RX path: rockblock webhook handler extracts packets, calls OnReceive.
// TX path: sends via constellation backend (Cloudloop/Rock7).
type IridiumInterface struct {
	mu        sync.RWMutex
	sender    SatelliteSender
	handler   PacketHandler
	available bool
}

// NewIridiumInterface creates an Iridium Reticulum transport interface.
// sender is the constellation backend for outbound MT messages.
func NewIridiumInterface(sender SatelliteSender) *IridiumInterface {
	return &IridiumInterface{
		sender:    sender,
		available: sender != nil,
	}
}

// Name returns the interface type.
func (i *IridiumInterface) Name() InterfaceType {
	return IfaceIridium
}

// Cost returns the per-message cost.
func (i *IridiumInterface) Cost() float64 {
	if i.sender != nil {
		return i.sender.CostPerMessage()
	}
	return InterfaceCost(IfaceIridium)
}

// MTU returns the maximum payload size. MO is 340 bytes, MT is 270 bytes.
// We return the smaller (MT) since that's the bottleneck for bidirectional comms.
func (i *IridiumInterface) MTU() int {
	if i.sender != nil {
		return i.sender.MaxPayload()
	}
	return 270 // Iridium MT MTU
}

// Send transmits a Reticulum packet via Iridium MT (satellite downlink).
// destID is the device IMEI.
func (i *IridiumInterface) Send(ctx context.Context, destID string, packet []byte) error {
	if i.sender == nil {
		return fmt.Errorf("iridium: no sender configured")
	}
	if len(packet) > i.MTU() {
		return fmt.Errorf("iridium: packet %d bytes exceeds MTU %d", len(packet), i.MTU())
	}

	slog.Debug("reticulum: sending packet via iridium",
		"dest", destID,
		"size", len(packet),
	)

	return i.sender.Send(ctx, destID, packet)
}

// IsAvailable returns true if the Iridium backend is operational.
func (i *IridiumInterface) IsAvailable() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.available && i.sender != nil
}

// SetAvailable updates the availability status (e.g. after connectivity check).
func (i *IridiumInterface) SetAvailable(avail bool) {
	i.mu.Lock()
	i.available = avail
	i.mu.Unlock()
}

// SetHandler registers a callback for inbound Reticulum packets received
// via the rockblock webhook (MO path).
func (i *IridiumInterface) SetHandler(h PacketHandler) {
	i.mu.Lock()
	i.handler = h
	i.mu.Unlock()
}

// OnReceive should be called by the rockblock webhook handler when a
// Reticulum packet is detected in an MO SBD payload. It dispatches
// to the registered PacketHandler.
func (i *IridiumInterface) OnReceive(raw []byte) {
	i.mu.RLock()
	h := i.handler
	i.mu.RUnlock()

	if h == nil {
		slog.Warn("reticulum: iridium packet received but no handler registered", "size", len(raw))
		return
	}

	slog.Debug("reticulum: iridium MO packet received", "size", len(raw))
	h(IfaceIridium, raw)
}
