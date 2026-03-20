package reticulum

import "context"

// Interface is the abstraction for a Reticulum transport interface.
// Each implementation wraps a specific transport (Iridium, MQTT, Tor, etc.)
// and provides bidirectional packet send/receive capabilities.
type Interface interface {
	// Name returns the interface type identifier.
	Name() InterfaceType

	// Cost returns the per-message cost for this interface.
	Cost() float64

	// MTU returns the maximum payload size in bytes.
	MTU() int

	// Send transmits a raw Reticulum packet via this interface.
	// The destination is identified by IMEI, peer ID, or similar
	// transport-specific addressing.
	Send(ctx context.Context, destID string, packet []byte) error

	// IsAvailable returns true if the interface is currently operational.
	IsAvailable() bool
}

// PacketHandler is called when a Reticulum packet is received on an interface.
// The router registers a handler with each interface to process inbound packets.
type PacketHandler func(iface InterfaceType, raw []byte)
