// Package fragment implements SBD message fragmentation and reassembly
// compatible with the meshsat bridge's fragment protocol.
//
// Fragment header (1 byte):
//
//	[MSG_ID:4bit | FRAG_NUM:2bit | FRAG_TOTAL:2bit]
//	- MSG_ID: 0-15 wrapping counter per device
//	- FRAG_NUM: 0-3 fragment index (0 = first)
//	- FRAG_TOTAL: 0-3 encoded as total-1 (1→0, 2→1, 3→2, 4→3)
//
// Max 4 fragments per message. Fragment payload = MTU - 1 (header byte).
package fragment

import (
	"fmt"
	"sync"
	"time"
)

// MTU constants for supported satellite constellations.
const (
	IridiumMO_MTU   = 340 // Iridium MO (Mobile Originated) max payload
	IridiumMT_MTU   = 270 // Iridium MT (Mobile Terminated) max payload
	AstrocastUL_MTU = 160 // Astrocast uplink max payload
	AstrocastDL_MTU = 40  // Astrocast downlink max payload
	HeaderSize      = 1   // 1-byte fragment header
	MaxFragments    = 4   // max fragments per message (2-bit field)
)

// EncodeHeader encodes a fragment header byte.
func EncodeHeader(msgID, fragNum, fragTotal uint8) byte {
	return (msgID << 4) | ((fragNum & 0x03) << 2) | ((fragTotal - 1) & 0x03)
}

// DecodeHeader decodes a fragment header byte.
func DecodeHeader(b byte) (msgID, fragNum, fragTotal uint8) {
	msgID = b >> 4
	fragNum = (b >> 2) & 0x03
	fragTotal = (b & 0x03) + 1
	return
}

// Fragment splits a message into fragments that fit within the given MTU.
// Returns nil if the message fits in a single frame (no fragmentation needed).
// msgID should be a wrapping counter (0-15) per device.
func Fragment(data []byte, mtu int, msgID uint8) [][]byte {
	if len(data) <= mtu {
		return nil // no fragmentation needed
	}

	fragPayload := mtu - HeaderSize
	if fragPayload <= 0 {
		return nil
	}

	// Calculate number of fragments needed.
	nFrags := (len(data) + fragPayload - 1) / fragPayload
	if nFrags > MaxFragments {
		nFrags = MaxFragments // truncate to max
	}

	fragments := make([][]byte, 0, nFrags)
	maskedID := msgID & 0x0F

	for i := 0; i < nFrags; i++ {
		start := i * fragPayload
		end := start + fragPayload
		if end > len(data) {
			end = len(data)
		}

		frag := make([]byte, 1+end-start)
		frag[0] = EncodeHeader(maskedID, uint8(i), uint8(nFrags))
		copy(frag[1:], data[start:end])
		fragments = append(fragments, frag)
	}

	return fragments
}

// pendingMessage holds fragments being reassembled.
type pendingMessage struct {
	fragments [][]byte
	total     int
	received  int
	createdAt time.Time
}

// Reassembler collects fragments and reassembles complete messages.
// Thread-safe. Keyed by (deviceID, msgID) for per-device isolation.
type Reassembler struct {
	mu      sync.Mutex
	pending map[string]*pendingMessage // key: "deviceID:msgID"
	maxAge  time.Duration
}

// NewReassembler creates a fragment reassembler with the given expiry timeout.
func NewReassembler(maxAge time.Duration) *Reassembler {
	return &Reassembler{
		pending: make(map[string]*pendingMessage),
		maxAge:  maxAge,
	}
}

// AddFragment adds a fragment. Returns the reassembled message if complete, nil otherwise.
func (r *Reassembler) AddFragment(deviceID string, data []byte) ([]byte, error) {
	if len(data) < HeaderSize {
		return nil, fmt.Errorf("fragment too short: %d bytes", len(data))
	}

	msgID, fragNum, fragTotal := DecodeHeader(data[0])
	payload := data[HeaderSize:]
	key := fmt.Sprintf("%s:%d", deviceID, msgID)

	r.mu.Lock()
	defer r.mu.Unlock()

	pm, ok := r.pending[key]
	if !ok {
		pm = &pendingMessage{
			fragments: make([][]byte, fragTotal),
			total:     int(fragTotal),
			createdAt: time.Now(),
		}
		r.pending[key] = pm
	}

	if int(fragNum) >= pm.total {
		return nil, fmt.Errorf("fragment index %d >= total %d", fragNum, pm.total)
	}

	if pm.fragments[fragNum] == nil {
		pm.received++
	}
	pm.fragments[fragNum] = payload

	if pm.received < pm.total {
		return nil, nil // not yet complete
	}

	// All fragments received — reassemble.
	var total int
	for _, f := range pm.fragments {
		total += len(f)
	}
	result := make([]byte, 0, total)
	for _, f := range pm.fragments {
		result = append(result, f...)
	}
	delete(r.pending, key)
	return result, nil
}

// Expire removes pending reassemblies older than maxAge.
// Call periodically (e.g., every 30s) to prevent memory leaks.
func (r *Reassembler) Expire() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Now().Add(-r.maxAge)
	expired := 0
	for key, pm := range r.pending {
		if pm.createdAt.Before(cutoff) {
			delete(r.pending, key)
			expired++
		}
	}
	return expired
}

// PendingCount returns the number of pending reassemblies.
func (r *Reassembler) PendingCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}
