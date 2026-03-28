package protocol

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"testing"
)

func TestGFMulIdentity(t *testing.T) {
	// a * 1 = a for all a.
	for a := 0; a < 256; a++ {
		result := GFMul(byte(a), 1)
		if result != byte(a) {
			t.Fatalf("GFMul(%d, 1) = %d, want %d", a, result, a)
		}
	}
}

func TestGFMulZero(t *testing.T) {
	// a * 0 = 0 for all a.
	for a := 0; a < 256; a++ {
		if GFMul(byte(a), 0) != 0 {
			t.Fatalf("GFMul(%d, 0) != 0", a)
		}
	}
}

func TestGFInvRoundTrip(t *testing.T) {
	// a * inv(a) = 1 for all a != 0.
	for a := 1; a < 256; a++ {
		inv := GFInv(byte(a))
		product := GFMul(byte(a), inv)
		if product != 1 {
			t.Fatalf("GFMul(%d, GFInv(%d)) = %d, want 1", a, a, product)
		}
	}
}

func TestGaussianEliminateIdentity(t *testing.T) {
	// Identity matrix should return payloads unchanged.
	k := 3
	payloadSize := 10
	coeffs := NewGFMatrix(k, k)
	for i := 0; i < k; i++ {
		coeffs.Set(i, i, 1)
	}

	payloads := make([][]byte, k)
	for i := 0; i < k; i++ {
		payloads[i] = make([]byte, payloadSize)
		rand.Read(payloads[i])
	}

	decoded, err := GaussianEliminate(coeffs, payloads)
	if err != nil {
		t.Fatalf("GaussianEliminate: %v", err)
	}

	for i := 0; i < k; i++ {
		if !bytes.Equal(decoded[i], payloads[i]) {
			t.Fatalf("segment %d mismatch", i)
		}
	}
}

func TestRLNCPacketMarshalRoundTrip(t *testing.T) {
	var hash [32]byte
	rand.Read(hash[:])

	pkt := &RLNCCodedPacket{
		ResourceHash: hash,
		Version:      RLNCVersion,
		GenerationID: 7,
		K:            3,
		Coefficients: []byte{0x11, 0x22, 0x33},
		Payload:      []byte("coded payload data"),
	}

	data := MarshalRLNCPacket(pkt)
	parsed, err := UnmarshalRLNCPacket(data)
	if err != nil {
		t.Fatalf("UnmarshalRLNCPacket: %v", err)
	}

	if parsed.ResourceHash != hash {
		t.Fatal("hash mismatch")
	}
	if parsed.Version != RLNCVersion {
		t.Fatal("version mismatch")
	}
	if parsed.GenerationID != 7 {
		t.Fatal("generation ID mismatch")
	}
	if parsed.K != 3 {
		t.Fatal("K mismatch")
	}
	if !bytes.Equal(parsed.Coefficients, pkt.Coefficients) {
		t.Fatal("coefficients mismatch")
	}
	if !bytes.Equal(parsed.Payload, pkt.Payload) {
		t.Fatal("payload mismatch")
	}
}

func TestRLNCPacketInvalid(t *testing.T) {
	// Too short.
	if _, err := UnmarshalRLNCPacket(make([]byte, 5)); err == nil {
		t.Fatal("expected error for short data")
	}

	// Bad version.
	data := make([]byte, 40)
	data[32] = 0xFF // version byte
	data[35] = 1    // K=1
	if _, err := UnmarshalRLNCPacket(data); err == nil {
		t.Fatal("expected error for bad version")
	}
}

func TestRLNCEncodeDecodeRoundTrip(t *testing.T) {
	// Create K=3 original segments.
	segments := [][]byte{
		[]byte("segment-one-data"),
		[]byte("segment-two-data"),
		[]byte("segment-333-dat"),
	}
	// Pad to uniform length (the longest).
	maxLen := 0
	for _, s := range segments {
		if len(s) > maxLen {
			maxLen = len(s)
		}
	}
	for i := range segments {
		if len(segments[i]) < maxLen {
			padded := make([]byte, maxLen)
			copy(padded, segments[i])
			segments[i] = padded
		}
	}

	hash := sha256.Sum256([]byte("test-resource"))
	packets := EncodeGeneration(1, hash, segments, 1.5)

	if len(packets) < 3 {
		t.Fatalf("expected at least 3 packets, got %d", len(packets))
	}

	// Verify all packets have correct metadata.
	for i, pkt := range packets {
		if pkt.ResourceHash != hash {
			t.Fatalf("packet %d: hash mismatch", i)
		}
		if pkt.K != 3 {
			t.Fatalf("packet %d: K=%d, want 3", i, pkt.K)
		}
		if pkt.GenerationID != 1 {
			t.Fatalf("packet %d: genID=%d, want 1", i, pkt.GenerationID)
		}
	}

	// Decode using just the first K packets.
	gen := NewRLNCGeneration(1, 3, maxLen)
	for _, pkt := range packets[:3] {
		gen.AddPacket(pkt)
	}

	decoded, err := gen.TryDecode()
	if err != nil {
		t.Fatalf("TryDecode: %v", err)
	}

	for i, seg := range decoded {
		if !bytes.Equal(seg, segments[i]) {
			t.Fatalf("segment %d mismatch after decode", i)
		}
	}
}

func TestRLNCGenerationNotEnoughPackets(t *testing.T) {
	gen := NewRLNCGeneration(1, 3, 10)
	gen.AddPacket(&RLNCCodedPacket{K: 3, Coefficients: []byte{1, 0, 0}, Payload: make([]byte, 10)})

	_, err := gen.TryDecode()
	if err == nil {
		t.Fatal("expected error for insufficient packets")
	}
}

func TestRLNCMarshalRoundTripLargePayload(t *testing.T) {
	var hash [32]byte
	rand.Read(hash[:])

	payload := make([]byte, 1000)
	rand.Read(payload)

	pkt := &RLNCCodedPacket{
		ResourceHash: hash,
		Version:      RLNCVersion,
		GenerationID: 42,
		K:            4,
		Coefficients: []byte{0xAA, 0xBB, 0xCC, 0xDD},
		Payload:      payload,
	}

	data := MarshalRLNCPacket(pkt)
	parsed, err := UnmarshalRLNCPacket(data)
	if err != nil {
		t.Fatalf("UnmarshalRLNCPacket: %v", err)
	}
	if !bytes.Equal(parsed.Payload, payload) {
		t.Fatal("large payload mismatch")
	}
}
