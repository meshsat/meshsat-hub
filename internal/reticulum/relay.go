package reticulum

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// RelayConfig controls the relay's forwarding behavior.
type RelayConfig struct {
	// MaxPacketsPerSec is the global rate limit for relayed packets.
	MaxPacketsPerSec int

	// RequireCreditsForPaid controls whether relay refuses to forward to
	// paid interfaces (Iridium, Astrocast, Globalstar) without credits.
	RequireCreditsForPaid bool
}

// DefaultRelayConfig returns sensible relay defaults.
func DefaultRelayConfig() RelayConfig {
	return RelayConfig{
		MaxPacketsPerSec:      100,
		RequireCreditsForPaid: true,
	}
}

// Relay forwards Reticulum packets between interfaces that can't reach
// each other directly. It looks up the destination in the routing table
// and sends via the best available interface.
type Relay struct {
	mu         sync.RWMutex
	router     *Router
	interfaces map[InterfaceType]Interface
	config     RelayConfig
	stats      RelayStats

	// Simple token-bucket rate limiter.
	tokens   atomic.Int64
	lastFill time.Time
	fillMu   sync.Mutex
}

// RelayStats tracks relay activity counters.
type RelayStats struct {
	Forwarded atomic.Int64
	Dropped   atomic.Int64
	NoRoute   atomic.Int64
	RateLimit atomic.Int64
}

// RelayStatsSnapshot is a JSON-friendly snapshot of relay counters.
type RelayStatsSnapshot struct {
	Forwarded int64 `json:"forwarded"`
	Dropped   int64 `json:"dropped"`
	NoRoute   int64 `json:"no_route"`
	RateLimit int64 `json:"rate_limited"`
}

// NewRelay creates a packet relay with the given routing table and config.
func NewRelay(router *Router, cfg RelayConfig) *Relay {
	r := &Relay{
		router:     router,
		interfaces: make(map[InterfaceType]Interface),
		config:     cfg,
		lastFill:   time.Now(),
	}
	r.tokens.Store(int64(cfg.MaxPacketsPerSec))
	return r
}

// RegisterInterface adds a transport interface to the relay.
func (r *Relay) RegisterInterface(iface Interface) {
	r.mu.Lock()
	r.interfaces[iface.Name()] = iface
	r.mu.Unlock()

	slog.Info("reticulum: relay registered interface",
		"iface", iface.Name(),
		"cost", iface.Cost(),
		"mtu", iface.MTU(),
	)
}

// Forward attempts to relay a packet to its destination via the best
// available interface. Returns nil if forwarded, error if dropped.
//
// sourceIface is the interface the packet arrived on (to avoid loops).
func (r *Relay) Forward(ctx context.Context, sourceIface InterfaceType, raw []byte) error {
	// Rate limit check.
	if !r.allowPacket() {
		r.stats.RateLimit.Add(1)
		return fmt.Errorf("relay: rate limited")
	}

	// Parse packet header to get destination and hop count.
	hdr, err := UnmarshalHeader(raw)
	if err != nil {
		r.stats.Dropped.Add(1)
		return fmt.Errorf("relay: invalid packet: %w", err)
	}

	// Check hop count.
	if !hdr.IncrementHop() {
		r.stats.Dropped.Add(1)
		slog.Debug("reticulum: relay dropped packet (max hops)",
			"dest", hex.EncodeToString(hdr.DestHash[:]),
		)
		return fmt.Errorf("relay: max hops exceeded")
	}

	// Look up destination in routing table.
	route := r.router.Lookup(hdr.DestHash)
	if route == nil {
		r.stats.NoRoute.Add(1)
		return fmt.Errorf("relay: no route for %s", hex.EncodeToString(hdr.DestHash[:]))
	}

	// Don't forward back to the same interface (loop prevention).
	if route.Interface == sourceIface {
		r.stats.Dropped.Add(1)
		return fmt.Errorf("relay: would loop back to source interface %s", sourceIface)
	}

	// Get the outbound interface.
	r.mu.RLock()
	outIface, ok := r.interfaces[route.Interface]
	r.mu.RUnlock()

	if !ok || !outIface.IsAvailable() {
		r.stats.Dropped.Add(1)
		return fmt.Errorf("relay: outbound interface %s not available", route.Interface)
	}

	// Re-serialize with incremented hop count.
	forwarded := hdr.Marshal()

	// Check outbound MTU.
	if len(forwarded) > outIface.MTU() {
		r.stats.Dropped.Add(1)
		return fmt.Errorf("relay: packet %d bytes exceeds %s MTU %d", len(forwarded), route.Interface, outIface.MTU())
	}

	// Send via outbound interface.
	// Use dest hash hex as the destination ID for satellite interfaces (IMEI lookup is higher level).
	destID := hex.EncodeToString(hdr.DestHash[:])
	if err := outIface.Send(ctx, destID, forwarded); err != nil {
		r.stats.Dropped.Add(1)
		return fmt.Errorf("relay: send via %s: %w", route.Interface, err)
	}

	r.stats.Forwarded.Add(1)
	slog.Debug("reticulum: packet relayed",
		"dest", destID,
		"from", sourceIface,
		"to", route.Interface,
		"hops", hdr.Hops,
		"size", len(forwarded),
	)

	return nil
}

// Broadcast sends a raw packet to all registered interfaces EXCEPT the source.
// Used for flooding announces to all transport interfaces (Reticulum spec behavior).
func (r *Relay) Broadcast(ctx context.Context, sourceIface InterfaceType, raw []byte) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for name, iface := range r.interfaces {
		if name == sourceIface {
			continue // don't send back to source
		}
		if !iface.IsAvailable() {
			continue
		}
		if len(raw) > iface.MTU() {
			continue
		}
		if err := iface.Send(ctx, "", raw); err != nil {
			slog.Debug("reticulum: broadcast send failed",
				"iface", name, "error", err)
		} else {
			slog.Debug("reticulum: broadcast sent",
				"from", sourceIface, "to", name, "size", len(raw))
		}
	}
}

// ListInterfaces returns all registered interfaces.
func (r *Relay) ListInterfaces() []Interface {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ifaces := make([]Interface, 0, len(r.interfaces))
	for _, i := range r.interfaces {
		ifaces = append(ifaces, i)
	}
	return ifaces
}

// Stats returns a snapshot of relay counters.
func (r *Relay) Stats() RelayStatsSnapshot {
	return RelayStatsSnapshot{
		Forwarded: r.stats.Forwarded.Load(),
		Dropped:   r.stats.Dropped.Load(),
		NoRoute:   r.stats.NoRoute.Load(),
		RateLimit: r.stats.RateLimit.Load(),
	}
}

// allowPacket implements a simple token-bucket rate limiter.
func (r *Relay) allowPacket() bool {
	r.fillMu.Lock()
	defer r.fillMu.Unlock()

	now := time.Now()
	elapsed := now.Sub(r.lastFill)

	// Refill tokens (1 token per 1/MaxPacketsPerSec).
	if elapsed > 0 && r.config.MaxPacketsPerSec > 0 {
		newTokens := int64(elapsed.Seconds() * float64(r.config.MaxPacketsPerSec))
		if newTokens > 0 {
			current := r.tokens.Load()
			max := int64(r.config.MaxPacketsPerSec)
			if current+newTokens > max {
				r.tokens.Store(max)
			} else {
				r.tokens.Add(newTokens)
			}
			r.lastFill = now
		}
	}

	// Try to consume a token.
	for {
		current := r.tokens.Load()
		if current <= 0 {
			return false
		}
		if r.tokens.CompareAndSwap(current, current-1) {
			return true
		}
	}
}
