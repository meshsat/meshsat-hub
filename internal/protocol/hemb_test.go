package protocol

import (
	"bytes"
	"testing"
)

func TestHeMBGF256MulInverse(t *testing.T) {
	for a := byte(1); a != 0; a++ {
		inv := hembGFInv(a)
		product := hembGFMul(a, inv)
		if product != 1 {
			t.Fatalf("gfMul(%d, gfInv(%d)) = %d, want 1", a, a, product)
		}
	}
}

func TestHeMBEncodeDecodeRoundtrip(t *testing.T) {
	// Simulate a K=3 generation with 4 symbols (N=4).
	segments := [][]byte{
		{0x01, 0x02, 0x03, 0x04},
		{0x05, 0x06, 0x07, 0x08},
		{0x09, 0x0A, 0x0B, 0x0C},
	}
	k := len(segments)

	// Generate coded symbols with known coefficients.
	coeffSets := [][]byte{
		{1, 0, 0}, // identity row 0
		{0, 1, 0}, // identity row 1
		{0, 0, 1}, // identity row 2
		{1, 1, 1}, // repair symbol
	}

	var symbols []HeMBCodedSymbol
	for i, coeffs := range coeffSets {
		data := make([]byte, len(segments[0]))
		for j, c := range coeffs {
			for b := 0; b < len(data); b++ {
				data[b] = hembGFAdd(data[b], hembGFMul(c, segments[j][b]))
			}
		}
		symbols = append(symbols, HeMBCodedSymbol{
			GenID:        0,
			SymbolIndex:  i,
			K:            k,
			Coefficients: coeffs,
			Data:         data,
		})
	}

	// Decode using only the first K symbols (identity matrix = trivial decode).
	decoded, err := HeMBTryDecode(symbols[:k], k)
	if err != nil {
		t.Fatal(err)
	}
	for i, seg := range decoded {
		if !bytes.Equal(seg, segments[i]) {
			t.Errorf("segment %d: got %v, want %v", i, seg, segments[i])
		}
	}

	// Decode using symbols 1,2,3 (skip first, use repair).
	decoded2, err := HeMBTryDecode(symbols[1:], k)
	if err != nil {
		t.Fatal(err)
	}
	for i, seg := range decoded2 {
		if !bytes.Equal(seg, segments[i]) {
			t.Errorf("repair decode segment %d: got %v, want %v", i, seg, segments[i])
		}
	}
}

func TestHeMBCRC8(t *testing.T) {
	data := []byte{0x48, 0x4D, 0x00, 0x01, 0x02, 0x03, 0x04}
	crc := hembCRC8(data)
	if hembCRC8(data) != crc {
		t.Error("CRC-8 not deterministic")
	}
	// Flip a bit — CRC should change.
	data[3] ^= 0x01
	if hembCRC8(data) == crc {
		t.Error("CRC-8 did not detect bit flip")
	}
}

func TestHeMBIsFrame(t *testing.T) {
	// Valid extended header.
	var ext [HeMBExtendedHeaderLen]byte
	ext[0] = HeMBMagicByte0
	ext[1] = HeMBMagicByte1
	ext[2] = 0x00 // version=0, streamID=0, flags=0
	ext[6] = 1    // K=1
	ext[7] = 1    // N=1
	ext[15] = hembCRC8(ext[:15])

	if !IsHeMBFrame(ext[:]) {
		t.Error("valid extended header not detected")
	}

	// Invalid magic.
	ext[0] = 0x00
	ext[15] = hembCRC8(ext[:15])
	if IsHeMBFrame(ext[:]) {
		t.Error("invalid magic should not be detected as HeMB")
	}
}

func TestHeMBReassemblyRoundtrip(t *testing.T) {
	var delivered []byte
	buf := NewHeMBReassemblyBuffer(func(streamID uint8, payload []byte) {
		delivered = make([]byte, len(payload))
		copy(delivered, payload)
	})

	segments := [][]byte{
		{0xAA, 0xBB},
		{0xCC, 0xDD},
	}
	k := 2

	// Build 2 identity symbols.
	for i := 0; i < k; i++ {
		coeffs := make([]byte, k)
		coeffs[i] = 1
		data := make([]byte, len(segments[0]))
		for j, c := range coeffs {
			for b := range data {
				data[b] = hembGFAdd(data[b], hembGFMul(c, segments[j][b]))
			}
		}
		_, err := buf.AddSymbol(0, uint8(i), HeMBCodedSymbol{
			GenID:        0,
			SymbolIndex:  i,
			K:            k,
			Coefficients: coeffs,
			Data:         data,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	expected := append(segments[0], segments[1]...)
	if !bytes.Equal(delivered, expected) {
		t.Errorf("delivered %v, want %v", delivered, expected)
	}
}

func TestHeMBGenerationCleanupAfterDecode(t *testing.T) {
	var delivered int
	buf := NewHeMBReassemblyBuffer(func(_ uint8, _ []byte) { delivered++ })

	// K=1: single identity symbol decodes immediately
	_, _ = buf.AddSymbol(5, 0, HeMBCodedSymbol{GenID: 0, K: 1, Coefficients: []byte{1}, Data: []byte{0xAA}})
	if delivered != 1 {
		t.Fatalf("expected 1 delivery, got %d", delivered)
	}
	// Generation should be cleaned up — stream removed
	if buf.Stats().ActiveStreams != 0 {
		t.Fatalf("expected 0 active streams after decode cleanup, got %d", buf.Stats().ActiveStreams)
	}
	// Reuse same stream+gen ID — must NOT hit "already decoded"
	_, _ = buf.AddSymbol(5, 0, HeMBCodedSymbol{GenID: 0, K: 1, Coefficients: []byte{1}, Data: []byte{0xBB}})
	if delivered != 2 {
		t.Fatalf("expected 2 deliveries (reused stream ID), got %d", delivered)
	}
}

func TestHeMBReassemblyReap(t *testing.T) {
	buf := NewHeMBReassemblyBuffer(nil)
	buf.maxAge = 0 // expire immediately

	_, _ = buf.AddSymbol(0, 0, HeMBCodedSymbol{K: 2, Coefficients: []byte{1, 0}, Data: []byte{0x01}})
	if buf.Stats().ActiveStreams != 1 {
		t.Errorf("expected 1 active stream, got %d", buf.Stats().ActiveStreams)
	}

	removed := buf.Reap()
	if removed != 1 {
		t.Errorf("expected 1 reaped, got %d", removed)
	}
	if buf.Stats().ActiveStreams != 0 {
		t.Errorf("expected 0 streams after reap, got %d", buf.Stats().ActiveStreams)
	}
}
