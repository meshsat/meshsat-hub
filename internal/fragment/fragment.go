// Package fragment implements SBD message fragmentation and reassembly
// compatible with the meshsat bridge's SBD fragment protocol.
//
// Fragment header (2 bytes):
//
//	Byte 0: [FRAG_INDEX:4bit | FRAG_TOTAL:4bit]
//	  - FRAG_INDEX: 0-15 fragment index (0 = first)
//	  - FRAG_TOTAL: 1-16 total fragments (encoded as total-1, so 0=1, 15=16)
//	Byte 1: MSG_ID (uint8, wrapping counter per device)
//	Bytes 2+: payload
//
// Fragment payload = MTU - 2 (header bytes).
package fragment

import (
	"fmt"
	"sync"
	"time"
)

// MTU constants for supported satellite constellations.
const (
	IridiumMO_MTU = 340 // Iridium MO (Mobile Originated) max payload
	IridiumMT_MTU = 270 // Iridium MT (Mobile Terminated) max payload
	HeaderSize    = 2   // 2-byte fragment header
	MaxFragments  = 16  // max fragments per message (4-bit field)
)

// EncodeHeader encodes the 2-byte fragment header.
func EncodeHeader(fragIndex, fragTotal, msgID uint8) [2]byte {
	return [2]byte{
		(fragIndex&0x0F)<<4 | ((fragTotal - 1) & 0x0F),
		msgID,
	}
}

// DecodeHeader decodes the 2-byte fragment header.
func DecodeHeader(b0, b1 byte) (fragIndex, fragTotal, msgID uint8) {
	fragIndex = b0 >> 4
	fragTotal = (b0 & 0x0F) + 1
	msgID = b1
	return
}

// MinFragmentPayload is the minimum payload size (after header) for a fragment
// to be considered valid. Real SBD fragments carry substantial payloads — the
// smallest reasonable fragment is from a 2-fragment split of a message just over
// the 340-byte MO MTU, giving ~170 bytes per fragment. Set to 100 to give margin
// while rejecting small messages whose random bytes look like fragment headers.
const MinFragmentPayload = 100

// IsFragment returns true if the payload looks like a fragmented message.
// Checks: fragTotal > 1, fragIndex < fragTotal, and payload size is reasonable.
func IsFragment(data []byte) bool {
	if len(data) < HeaderSize+MinFragmentPayload {
		return false
	}
	fragIndex, fragTotal, _ := DecodeHeader(data[0], data[1])
	return fragTotal > 1 && fragIndex < fragTotal
}

// Fragment splits a message into fragments that fit within the given MTU.
// Returns nil if the message fits in a single frame (no fragmentation needed).
// msgID should be a wrapping counter (0-255) per device.
func Fragment(data []byte, mtu int, msgID uint8) [][]byte {
	if len(data) <= mtu {
		return nil // no fragmentation needed
	}

	fragPayload := mtu - HeaderSize
	if fragPayload <= 0 {
		return nil
	}

	nFrags := (len(data) + fragPayload - 1) / fragPayload
	if nFrags > MaxFragments {
		nFrags = MaxFragments
		data = data[:nFrags*fragPayload]
	}

	fragments := make([][]byte, 0, nFrags)
	for i := 0; i < nFrags; i++ {
		start := i * fragPayload
		end := start + fragPayload
		if end > len(data) {
			end = len(data)
		}

		hdr := EncodeHeader(uint8(i), uint8(nFrags), msgID)
		frag := make([]byte, HeaderSize+end-start)
		frag[0] = hdr[0]
		frag[1] = hdr[1]
		copy(frag[HeaderSize:], data[start:end])
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

	fragIndex, fragTotal, msgID := DecodeHeader(data[0], data[1])
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

	if int(fragIndex) >= pm.total {
		return nil, fmt.Errorf("fragment index %d >= total %d", fragIndex, pm.total)
	}

	if pm.fragments[fragIndex] == nil {
		pm.received++
	}
	pm.fragments[fragIndex] = payload

	if pm.received < pm.total {
		return nil, nil // not yet complete
	}

	// All fragments received — reassemble in order.
	var totalLen int
	for _, f := range pm.fragments {
		totalLen += len(f)
	}
	result := make([]byte, 0, totalLen)
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
