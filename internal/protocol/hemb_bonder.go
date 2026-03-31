package protocol

// HeMB bonder — Hub-side RLNC encode + send. [MESHSAT-446]
// Distributes coded symbols across Hub webhook bearers (Twilio, Rock7, Cloudloop).

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sort"
	"sync/atomic"
)

var globalStreamSeq atomic.Uint32

// HeMBHubBearer is a bearer profile with a send function for Hub-side bonded sends.
type HeMBHubBearer struct {
	HeMBBearerProfile
	SendFn func(ctx context.Context, data []byte) error
}

// HeMBBonderOpts configures a Hub-side HeMB bonder.
type HeMBBonderOpts struct {
	Bearers []HeMBHubBearer
}

// HeMBBondSend performs an RLNC-coded bonded send across Hub webhook bearers.
// Encodes the payload into coded symbols, distributes across bearers with
// cost-weighted allocation (free first), and sends via each bearer's SendFn.
func HeMBBondSend(ctx context.Context, opts HeMBBonderOpts, payload []byte) (int, error) {
	bearers := opts.Bearers
	if len(bearers) == 0 {
		return 0, fmt.Errorf("hemb: no bearers configured")
	}

	// Single bearer: passthrough (no RLNC overhead).
	if len(bearers) == 1 {
		frame := marshalPassthrough(&bearers[0], payload)
		if err := bearers[0].SendFn(ctx, frame); err != nil {
			return 0, err
		}
		return 1, nil
	}

	// Sort: free first (by MTU DESC), then paid (by cost ASC).
	sort.Slice(bearers, func(i, j int) bool {
		fi, fj := bearers[i].IsFree(), bearers[j].IsFree()
		if fi != fj {
			return fi
		}
		if fi {
			return bearers[i].MTU > bearers[j].MTU
		}
		return bearers[i].CostPerMsg < bearers[j].CostPerMsg
	})

	streamID := uint8(globalStreamSeq.Add(1) & 0xFF)

	// Compute symbol size from smallest bearer MTU.
	minMTU := math.MaxInt
	for _, b := range bearers {
		m := b.MTU - HeMBExtendedHeaderLen
		if m < minMTU {
			minMTU = m
		}
	}
	if minMTU <= 2 {
		return 0, fmt.Errorf("hemb: bearer MTU too small")
	}

	// Estimate K, refine symbol size.
	roughSymSize := minMTU - 1
	if roughSymSize <= 0 {
		roughSymSize = 1
	}
	k := (len(payload) + roughSymSize - 1) / roughSymSize
	if k > 255 {
		k = 255
	}
	if k == 0 {
		k = 1
	}
	symSize := minMTU - k
	if symSize <= 0 {
		k = minMTU / 2
		if k == 0 {
			k = 1
		}
		symSize = minMTU - k
	}
	k = (len(payload) + symSize - 1) / symSize
	if k > 255 {
		k = 255
		symSize = (len(payload) + k - 1) / k
	}
	if k == 0 {
		k = 1
	}

	// Segment payload.
	segments := make([][]byte, k)
	for i := 0; i < k; i++ {
		start := i * symSize
		end := start + symSize
		if end > len(payload) {
			end = len(payload)
		}
		segments[i] = make([]byte, symSize)
		copy(segments[i], payload[start:end])
	}

	// Allocate symbols to bearers.
	type alloc struct {
		bearer *HeMBHubBearer
		source int
		repair int
		total  int
	}
	allocs := make([]alloc, len(bearers))
	remaining := k
	for i := range bearers {
		allocs[i].bearer = &bearers[i]
		if remaining > 0 && bearers[i].IsFree() {
			allocs[i].source = remaining
			remaining = 0
		}
	}
	for i := range bearers {
		if remaining > 0 && !bearers[i].IsFree() {
			allocs[i].source = remaining
			remaining = 0
		}
	}
	// Add repair symbols.
	for i := range allocs {
		if allocs[i].source > 0 {
			allocs[i].repair = HeMBRepairSymbols(&allocs[i].bearer.HeMBBearerProfile, allocs[i].source)
		}
		allocs[i].total = allocs[i].source + allocs[i].repair
	}

	totalN := 0
	for _, a := range allocs {
		totalN += a.total
	}

	// RLNC encode.
	symbols, err := hembEncodeGeneration(0, segments, totalN, rand.Reader)
	if err != nil {
		return 0, fmt.Errorf("hemb: encode: %w", err)
	}

	// Send symbols across bearers.
	si := 0
	var sendErr error
	for _, a := range allocs {
		for j := 0; j < a.total; j++ {
			sym := symbols[si]
			si++
			frame := marshalSymbolFrame(a.bearer, streamID, sym, totalN)
			if err := a.bearer.SendFn(ctx, frame); err != nil {
				slog.Warn("hemb: bearer send failed", "bearer", a.bearer.InterfaceID, "error", err)
				sendErr = err
			}
		}
	}

	return len(bearers), sendErr
}

// marshalPassthrough creates a single-bearer passthrough frame (N=1).
func marshalPassthrough(b *HeMBHubBearer, payload []byte) []byte {
	streamID := uint8(globalStreamSeq.Add(1) & 0xFF)
	hdr := MarshalHeMBExtended(HeMBExtendedHeader{
		StreamID:         streamID,
		Flags:            HeMBFlagData,
		K:                1,
		N:                1,
		BearerIndex:      b.Index,
		TotalPayloadSize: uint16(len(payload)),
	})
	frame := make([]byte, 0, HeMBExtendedHeaderLen+1+len(payload))
	frame = append(frame, hdr[:]...)
	frame = append(frame, 1) // coefficient = 1 (identity)
	frame = append(frame, payload...)
	return frame
}

// marshalSymbolFrame creates an extended HeMB frame for a coded symbol.
func marshalSymbolFrame(b *HeMBHubBearer, streamID uint8, sym HeMBCodedSymbol, totalN int) []byte {
	n := totalN
	if n > 255 {
		n = 255
	}
	hdr := MarshalHeMBExtended(HeMBExtendedHeader{
		StreamID:     streamID,
		Flags:        HeMBFlagData,
		Sequence:     uint16(sym.SymbolIndex),
		K:            uint8(sym.K),
		N:            uint8(n),
		BearerIndex:  b.Index,
		GenerationID: sym.GenID,
	})
	frame := make([]byte, 0, HeMBExtendedHeaderLen+len(sym.Coefficients)+len(sym.Data))
	frame = append(frame, hdr[:]...)
	frame = append(frame, sym.Coefficients...)
	frame = append(frame, sym.Data...)
	return frame
}

// hembEncodeGeneration produces N RLNC-coded symbols from K source segments.
func hembEncodeGeneration(genID uint16, segments [][]byte, n int, r io.Reader) ([]HeMBCodedSymbol, error) {
	k := len(segments)
	if k == 0 {
		return nil, nil
	}
	if n < k {
		return nil, fmt.Errorf("hemb: N=%d < K=%d", n, k)
	}
	if k > 255 {
		return nil, fmt.Errorf("hemb: K=%d exceeds 255", k)
	}

	payloadSize := 0
	for _, seg := range segments {
		if len(seg) > payloadSize {
			payloadSize = len(seg)
		}
	}
	padded := make([][]byte, k)
	for i, seg := range segments {
		padded[i] = make([]byte, payloadSize)
		copy(padded[i], seg)
	}

	symbols := make([]HeMBCodedSymbol, n)
	for i := 0; i < n; i++ {
		coefficients := make([]byte, k)
		if _, err := io.ReadFull(r, coefficients); err != nil {
			return nil, fmt.Errorf("hemb: random: %w", err)
		}
		allZero := true
		for _, c := range coefficients {
			if c != 0 {
				allZero = false
				break
			}
		}
		if allZero {
			coefficients[0] = 1
		}

		payload := make([]byte, payloadSize)
		for j := 0; j < k; j++ {
			if coefficients[j] == 0 {
				continue
			}
			for b := 0; b < payloadSize; b++ {
				payload[b] = hembGFAdd(payload[b], hembGFMul(coefficients[j], padded[j][b]))
			}
		}

		symbols[i] = HeMBCodedSymbol{
			GenID:        genID,
			SymbolIndex:  i,
			K:            k,
			Coefficients: coefficients,
			Data:         payload,
		}
	}
	return symbols, nil
}
