package reticulum

import (
	"context"
	"encoding/hex"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// PathHandlerConfig controls path request handling behavior.
type PathHandlerConfig struct {
	// ResponseDelay adds a small random delay before responding to avoid
	// collision when multiple nodes know the route.
	ResponseDelay time.Duration
}

// DefaultPathHandlerConfig returns sensible defaults.
func DefaultPathHandlerConfig() PathHandlerConfig {
	return PathHandlerConfig{
		ResponseDelay: 50 * time.Millisecond,
	}
}

// PathHandler processes incoming Reticulum path request packets and responds
// with routing information from the Hub's routing table. This makes the Hub
// a Super Transport Node that bridges can query for path discovery.
type PathHandler struct {
	router *Router
	relay  *Relay
	config PathHandlerConfig

	// Dedup: track recently seen tags to avoid duplicate responses.
	mu       sync.Mutex
	seenTags map[[TruncatedHashLen]byte]time.Time

	stats PathHandlerStats
}

// PathHandlerStats tracks path request/response activity.
type PathHandlerStats struct {
	RequestsReceived atomic.Int64
	ResponsesSent    atomic.Int64
	NoRoute          atomic.Int64
	Deduplicated     atomic.Int64
}

// PathHandlerStatsSnapshot is a JSON-friendly snapshot.
type PathHandlerStatsSnapshot struct {
	RequestsReceived int64 `json:"requests_received"`
	ResponsesSent    int64 `json:"responses_sent"`
	NoRoute          int64 `json:"no_route"`
	Deduplicated     int64 `json:"deduplicated"`
}

// NewPathHandler creates a path request handler using the given routing table.
func NewPathHandler(router *Router, relay *Relay, cfg PathHandlerConfig) *PathHandler {
	return &PathHandler{
		router:   router,
		relay:    relay,
		config:   cfg,
		seenTags: make(map[[TruncatedHashLen]byte]time.Time),
	}
}

// HandlePacket checks if a raw packet is a path request (ContextRequest) and
// responds if the Hub knows a route. Returns true if the packet was handled.
func (ph *PathHandler) HandlePacket(ctx context.Context, sourceIface InterfaceType, raw []byte) bool {
	hdr, err := UnmarshalHeader(raw)
	if err != nil {
		return false
	}

	// Only handle path requests (PacketData + DestPlain + ContextRequest).
	if hdr.PacketType != PacketData || hdr.DestType != DestPlain || hdr.Context != ContextRequest {
		return false
	}

	req, err := UnmarshalPathRequest(hdr.Data)
	if err != nil {
		slog.Debug("reticulum: invalid path request", "error", err)
		return true // consumed but invalid
	}

	ph.stats.RequestsReceived.Add(1)

	destHex := hex.EncodeToString(req.DestHash[:])
	slog.Debug("reticulum: path request received",
		"dest", destHex,
		"from", sourceIface,
	)

	// Dedup check — don't respond to the same tag twice.
	if ph.isDuplicate(req.Tag) {
		ph.stats.Deduplicated.Add(1)
		return true
	}

	// Look up the destination in the routing table.
	route := ph.router.Lookup(req.DestHash)
	if route == nil {
		ph.stats.NoRoute.Add(1)
		slog.Debug("reticulum: path request — no route known", "dest", destHex)
		return true
	}

	// Build and send path response back via the same interface.
	resp := &PathResponse{
		DestHash:      req.DestHash,
		Tag:           req.Tag,
		Hops:          byte(route.Hops),
		InterfaceType: string(route.Interface),
		AnnounceData:  route.AppData,
	}

	pkt := BuildPathResponsePacket(req.DestHash, resp)

	// Small delay to avoid collision with other responders.
	if ph.config.ResponseDelay > 0 {
		time.Sleep(ph.config.ResponseDelay)
	}

	// Send response back on the source interface.
	ph.sendResponse(ctx, sourceIface, pkt)

	ph.stats.ResponsesSent.Add(1)
	slog.Info("reticulum: path response sent",
		"dest", destHex,
		"via", route.Interface,
		"hops", route.Hops,
		"to", sourceIface,
	)

	return true
}

// sendResponse sends a path response packet via the given interface.
func (ph *PathHandler) sendResponse(ctx context.Context, iface InterfaceType, pkt []byte) {
	if ph.relay == nil {
		return
	}
	for _, i := range ph.relay.ListInterfaces() {
		if i.Name() == iface && i.IsAvailable() {
			if err := i.Send(ctx, "", pkt); err != nil {
				slog.Error("reticulum: path response send failed",
					"iface", iface,
					"error", err,
				)
			}
			return
		}
	}
	slog.Warn("reticulum: path response — source interface not found", "iface", iface)
}

// isDuplicate checks if we've already seen this tag recently.
func (ph *PathHandler) isDuplicate(tag [TruncatedHashLen]byte) bool {
	ph.mu.Lock()
	defer ph.mu.Unlock()

	now := time.Now()

	// Check if tag was seen in the last 30 seconds.
	if seen, ok := ph.seenTags[tag]; ok && now.Sub(seen) < 30*time.Second {
		return true
	}

	ph.seenTags[tag] = now
	return false
}

// PruneStale removes old entries from the dedup map. Call periodically.
func (ph *PathHandler) PruneStale() {
	ph.mu.Lock()
	defer ph.mu.Unlock()

	cutoff := time.Now().Add(-30 * time.Second)
	for tag, seen := range ph.seenTags {
		if seen.Before(cutoff) {
			delete(ph.seenTags, tag)
		}
	}
}

// Stats returns a snapshot of path handler activity.
func (ph *PathHandler) Stats() PathHandlerStatsSnapshot {
	return PathHandlerStatsSnapshot{
		RequestsReceived: ph.stats.RequestsReceived.Load(),
		ResponsesSent:    ph.stats.ResponsesSent.Load(),
		NoRoute:          ph.stats.NoRoute.Load(),
		Deduplicated:     ph.stats.Deduplicated.Load(),
	}
}
