package protocol

// HeMB reassembly buffer — ported from meshsat/internal/hemb/reassemble.go.
// Collects RLNC-coded symbols from multiple bearers and decodes when K
// independent symbols are received.

import (
	"log/slog"
	"sync"
	"time"
)

// HeMBReassemblyBuffer collects coded symbols and attempts decode.
type HeMBReassemblyBuffer struct {
	mu        sync.Mutex
	streams   map[uint8]*hembStreamState
	deliverFn func(streamID uint8, payload []byte) // called on successful decode
	maxAge    time.Duration
}

type hembStreamState struct {
	streamID    uint8
	generations map[uint16]*hembGenerationState
	createdAt   time.Time
}

type hembGenerationState struct {
	genID      uint16
	k          int
	symbols    []HeMBCodedSymbol
	bearerSeen map[uint8]bool
	firstSeen  time.Time
	decoded    bool
}

// NewHeMBReassemblyBuffer creates a reassembly buffer.
// deliverFn is called with the stream ID and decoded payload when a generation
// is successfully reassembled.
func NewHeMBReassemblyBuffer(deliverFn func(streamID uint8, payload []byte)) *HeMBReassemblyBuffer {
	return &HeMBReassemblyBuffer{
		streams:   make(map[uint8]*hembStreamState),
		deliverFn: deliverFn,
		maxAge:    5 * time.Minute,
	}
}

// AddSymbol processes an inbound coded symbol. Returns the reassembled payload
// when a generation is decoded, nil otherwise.
func (rb *HeMBReassemblyBuffer) AddSymbol(streamID uint8, bearerIndex uint8, sym HeMBCodedSymbol) ([]byte, error) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	stream, ok := rb.streams[streamID]
	if !ok {
		stream = &hembStreamState{
			streamID:    streamID,
			generations: make(map[uint16]*hembGenerationState),
			createdAt:   time.Now(),
		}
		rb.streams[streamID] = stream
	}

	gen, ok := stream.generations[sym.GenID]
	if !ok {
		gen = &hembGenerationState{
			genID:      sym.GenID,
			k:          sym.K,
			bearerSeen: make(map[uint8]bool),
			firstSeen:  time.Now(),
		}
		stream.generations[sym.GenID] = gen
	}

	if gen.decoded {
		return nil, nil
	}

	gen.symbols = append(gen.symbols, sym)
	gen.bearerSeen[bearerIndex] = true

	if len(gen.symbols) >= gen.k {
		return rb.tryDecode(streamID, gen)
	}

	return nil, nil
}

func (rb *HeMBReassemblyBuffer) tryDecode(streamID uint8, gen *hembGenerationState) ([]byte, error) {
	decoded, err := HeMBTryDecode(gen.symbols, gen.k)
	if err != nil {
		if isNotDecodable(err) {
			return nil, nil // rank deficient — wait for more symbols
		}
		slog.Debug("hemb: decode failed", "stream", streamID, "gen", gen.genID, "err", err)
		return nil, err
	}

	gen.decoded = true

	var payload []byte
	for _, seg := range decoded {
		payload = append(payload, seg...)
	}

	if rb.deliverFn != nil {
		rb.deliverFn(streamID, payload)
	}

	// Remove decoded generation to free stream+gen ID for reuse.
	if stream, ok := rb.streams[streamID]; ok {
		delete(stream.generations, gen.genID)
		if len(stream.generations) == 0 {
			delete(rb.streams, streamID)
		}
	}

	return payload, nil
}

func isNotDecodable(err error) bool {
	return err != nil && err.Error() == ErrHeMBNotDecodable.Error()
}

// Reap removes streams older than maxAge. Returns number of streams removed.
func (rb *HeMBReassemblyBuffer) Reap() int {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	removed := 0
	now := time.Now()
	for id, s := range rb.streams {
		if now.Sub(s.createdAt) > rb.maxAge {
			delete(rb.streams, id)
			removed++
		}
	}
	return removed
}

// Stats returns current reassembly buffer statistics.
func (rb *HeMBReassemblyBuffer) Stats() HeMBReassemblyStats {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	var s HeMBReassemblyStats
	for _, stream := range rb.streams {
		for _, gen := range stream.generations {
			if gen.decoded {
				s.GenerationsDecoded++
			} else {
				s.GenerationsPending++
			}
		}
	}
	s.ActiveStreams = len(rb.streams)
	return s
}

// HeMBReassemblyStats reports buffer state.
type HeMBReassemblyStats struct {
	ActiveStreams      int `json:"active_streams"`
	GenerationsDecoded int `json:"generations_decoded"`
	GenerationsPending int `json:"generations_pending"`
}
